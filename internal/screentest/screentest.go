// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package screentest implements script-based visual diff testing
// for webpages.
//
// Scripts
//
// A script is a text file containing a sequence of testcases, separated by
// blank lines. Lines beginning with # characters are ignored as comments. A
// testcase is a sequence of lines describing actions to take on a page, along
// with the dimensions of the screenshots to be compared. For example, here is
// a trivial script:
//
//  compare https://go.dev http://localhost:6060
//  pathname /about
//  capture fullscreen
//
// This script has a single testcase. The first line sets the origin servers to
// compare. The second line sets the page to visit at each origin. The last line
// captures fullpage screenshots of the pages and generates a diff image if they
// do not match.
//
// Keywords
//
// Use windowsize WIDTHxHEIGHT to set the default window size for all testcases
// that follow.
//
//  windowsize 540x1080
//
// Use compare ORIGIN ORIGIN to set the origins to compare.
//
//  compare https://go.dev http://localhost:6060
//
// Use test NAME to create a name for the testcase.
//
//  test about page
//
// Use pathname PATH to set the page to visit at each origin. If no
// test name is set, PATH will be used as the name for the test.
//
//  pathname /about
//
// Use click SELECTOR to add a click an element on the page.
//
//  click button.submit
//
// Use wait SELECTOR to wait for an element to appear.
//
//  wait [role="treeitem"][aria-expanded="true"]
//
// Use capture [SIZE] [ARG] to create a testcase with the properties
// defined above.
//
//  capture fullscreen 540x1080
//
// When taking an element screenshot provide a selector.
//
//  capture element header
//
// Chain capture commands to create multiple testcases for a single page.
//
//  windowsize 1536x960
//  compare https://go.dev http://localhost:6060
//
//  test homepage
//  pathname /
//  capture viewport
//  capture viewport 540x1080
//  capture viewport 400x1000
//
//  test about page
//  pathname /about
//  capture viewport
//  capture viewport 540x1080
//  capture viewport 400x1000
//
package screentest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/n7olkachev/imgdiff/pkg/imgdiff"
	"golang.org/x/sync/errgroup"
)

// CheckHandler runs the test scripts matched by glob. If any errors are
// encountered, CheckHandler returns an error listing the problems.
func CheckHandler(glob string) error {
	ctx := context.Background()
	files, err := filepath.Glob(glob)
	if err != nil {
		return fmt.Errorf("filepath.Glob(%q): %w", glob, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no files match %q", glob)
	}
	ctx, cancel := chromedp.NewExecAllocator(ctx, append(
		chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(browserWidth, browserHeight),
	)...)
	defer cancel()
	var buf bytes.Buffer
	for _, file := range files {
		tests, err := readTests(file)
		if err != nil {
			return fmt.Errorf("readTestdata(%q): %w", file, err)
		}
		if len(tests) == 0 {
			return fmt.Errorf("no tests found in %q", file)
		}
		ctx, cancel = chromedp.NewContext(ctx, chromedp.WithLogf(log.Printf))
		defer cancel()
		var hdr bool
		out, err := outDir(file)
		if err != nil {
			return fmt.Errorf("outDir(%q): %w", file, err)
		}
		for _, test := range tests {
			if err := runDiff(ctx, test, out); err != nil {
				if !hdr {
					fmt.Fprintf(&buf, "%s\n", file)
					fmt.Fprintf(&buf, "inspect diffs at %s\n", out)
					hdr = true
				}
				fmt.Fprintf(&buf, "%v\n", err)
			}
		}
	}
	if buf.Len() > 0 {
		return errors.New(buf.String())
	}
	return nil
}

// TestHandler runs the test script files matched by glob.
func TestHandler(t *testing.T, glob string) error {
	ctx := context.Background()
	files, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no files match %#q", glob)
	}
	ctx, cancel := chromedp.NewExecAllocator(ctx, append(
		chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(browserWidth, browserHeight),
	)...)
	defer cancel()
	for _, file := range files {
		tests, err := readTests(file)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel = chromedp.NewContext(ctx, chromedp.WithLogf(t.Logf))
		defer cancel()
		out, err := outDir(file)
		if err != nil {
			return fmt.Errorf("outDir(%q): %w", file, err)
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if err := runDiff(ctx, test, out); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	return nil
}

// outDir prepares a diff output directory for a given testfile.
// It empties the directory if it already exists.
func outDir(testfile string) (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("os.UserCacheDir(): %w", err)
	}
	out := filepath.Join(d, "screentest", sanitized(filepath.Base(testfile)))
	err = os.RemoveAll(out)
	if err != nil {
		return "", fmt.Errorf("os.RemoveAll(%q): %w", out, err)
	}
	err = os.MkdirAll(out, os.ModePerm)
	if err != nil {
		return "", fmt.Errorf("os.MkdirAll(%q): %w", out, err)
	}
	return out, nil
}

const (
	browserWidth  = 1536
	browserHeight = 960
)

var sanitize = regexp.MustCompile("[.*<>?`'|/\\: ]")

type screenshotType int

const (
	fullScreenshot screenshotType = iota
	viewportScreenshot
	elementScreenshot
)

type testcase struct {
	name              string
	pathame           string
	tasks             chromedp.Tasks
	originA           string
	originB           string
	viewportWidth     int
	viewportHeight    int
	screenshotType    screenshotType
	screenshotElement string
}

// readTests parses the testcases from a text file.
func readTests(file string) ([]*testcase, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tests []*testcase
	var (
		testName, pathname string
		tasks              chromedp.Tasks
		originA, originB   string
		width, height      int
		lineNo             int
	)
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		lineNo += 1
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimRight(line, " \t")
		field, args := splitOneField(line)
		field = strings.ToUpper(field)
		switch field {
		case "":
			// We've reached an empty line, reset properties scoped to a single test.
			testName = ""
			pathname = ""
			tasks = nil
		case "COMPARE":
			origins := strings.Split(args, " ")
			originA, originB = origins[0], origins[1]
			if _, err := url.Parse(originA); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originA, err)
			}
			if _, err := url.Parse(originB); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originB, err)
			}
		case "WINDOWSIZE":
			width, height, err = splitDimensions(args)
			if err != nil {
				return nil, fmt.Errorf("splitDimensions(%q): %w", args, err)
			}
		case "TEST":
			testName = args
			for _, t := range tests {
				if t.name == testName {
					return nil, fmt.Errorf(
						"duplicate test name %q on line %d", testName, lineNo)
				}
			}
		case "PATHNAME":
			if _, err := url.Parse(originA + args); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originA+args, err)
			}
			if _, err := url.Parse(originB + args); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originB+args, err)
			}
			pathname = args
			if testName == "" {
				testName = pathname
			}
			for _, t := range tests {
				if t.name == testName {
					return nil, fmt.Errorf(
						"duplicate test with pathname %q on line %d", pathname, lineNo)
				}
			}
		case "CLICK":
			tasks = append(tasks, chromedp.Click(args))
		case "WAIT":
			tasks = append(tasks, chromedp.WaitReady(args))
		case "CAPTURE":
			if originA == "" || originB == "" {
				return nil, fmt.Errorf("missing compare for capture on line %d", lineNo)
			}
			if pathname == "" {
				return nil, fmt.Errorf("missing pathname for capture on line %d", lineNo)
			}
			test := &testcase{
				name:    testName,
				pathame: pathname,
				tasks:   tasks,
				originA: originA,
				originB: originB,
				// Default to viewportScreenshot
				screenshotType: viewportScreenshot,
				viewportWidth:  width,
				viewportHeight: height,
			}
			tests = append(tests, test)
			field, args := splitOneField(args)
			field = strings.ToUpper(field)
			switch field {
			case "FULLSCREEN", "VIEWPORT":
				if field == "FULLSCREEN" {
					test.screenshotType = fullScreenshot
				}
				if args != "" {
					w, h, err := splitDimensions(args)
					if err != nil {
						return nil, fmt.Errorf("splitDimensions(%q): %w", args, err)
					}
					test.name = testName + fmt.Sprintf(" %dx%d", w, h)
					test.viewportWidth = w
					test.viewportHeight = h
				}
			case "ELEMENT":
				test.name = testName + fmt.Sprintf(" %s", args)
				test.screenshotType = elementScreenshot
				test.screenshotElement = args
			}
		default:
			// We should never reach this error.
			return nil, fmt.Errorf("invalid syntax on line %d: %q", lineNo, line)
		}
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("scan.Err(): %v", err)
	}
	return tests, nil
}

// splitOneField splits text at the first space or tab
// and returns that first field and the remaining text.
func splitOneField(text string) (field, rest string) {
	i := strings.IndexAny(text, " \t")
	if i < 0 {
		return text, ""
	}
	return text[:i], strings.TrimLeft(text[i:], " \t")
}

// splitDimensions parses a window dimension string into int values
// for width and height.
func splitDimensions(text string) (width, height int, err error) {
	windowsize := strings.Split(text, "x")
	if len(windowsize) != 2 {
		return width, height, fmt.Errorf("syntax error: windowsize %s", text)
	}
	width, err = strconv.Atoi(windowsize[0])
	if err != nil {
		return width, height, fmt.Errorf("strconv.Atoi(%q): %w", windowsize[0], err)
	}
	height, err = strconv.Atoi(windowsize[1])
	if err != nil {
		return width, height, fmt.Errorf("strconv.Atoi(%q): %w", windowsize[1], err)
	}
	return width, height, nil
}

// runDiff generates screenshots for a given test case and
// a diff if the screenshots do not match.
func runDiff(ctx context.Context, test *testcase, out string) error {
	fmt.Printf("test %s\n", test.name)
	urlA, err := url.Parse(test.originA + test.pathame)
	if err != nil {
		return fmt.Errorf("url.Parse(%q): %w", test.originA+test.pathame, err)
	}
	urlB, err := url.Parse(test.originB + test.pathame)
	if err != nil {
		return fmt.Errorf("url.Parse(%q): %w", test.originB+test.pathame, err)
	}
	screenA, err := captureScreenshot(ctx, urlA, test)
	if err != nil {
		return fmt.Errorf("fullScreenshot(ctx, %q, %q): %w", urlA, test, err)
	}
	screenB, err := captureScreenshot(ctx, urlB, test)
	if err != nil {
		return fmt.Errorf("fullScreenshot(ctx, %q, %q): %w", urlB, test, err)
	}
	if bytes.Equal(screenA, screenB) {
		fmt.Printf("%s == %s\n\n", urlA, urlB)
		return nil
	}
	fmt.Printf("%s != %s\n", urlA, urlB)
	imgA, _, err := image.Decode(bytes.NewReader(screenA))
	if err != nil {
		return fmt.Errorf("image.Decode(...): %w", err)
	}
	imgB, _, err := image.Decode(bytes.NewReader(screenB))
	if err != nil {
		return fmt.Errorf("image.Decode(...): %w", err)
	}
	outfile := filepath.Join(out, sanitized(test.name))
	var errs errgroup.Group
	errs.Go(func() error {
		out := imgdiff.Diff(imgA, imgB, &imgdiff.Options{
			Threshold: 0.1,
			DiffImage: true,
		})
		return writePNG(&out.Image, outfile+".diff")
	})
	errs.Go(func() error {
		return writePNG(&imgA, outfile+"."+sanitized(urlA.Host))
	})
	errs.Go(func() error {
		return writePNG(&imgB, outfile+"."+sanitized(urlB.Host))
	})
	if err := errs.Wait(); err != nil {
		return fmt.Errorf("writePNG(...): %w", errs.Wait())
	}
	fmt.Printf("wrote diff to %s\n\n", out)
	return fmt.Errorf("%s != %s", urlA, urlB)
}

// captureScreenshot runs a series of browser actions and takes a screenshot
// of the resulting webpage in an instance of headless chrome.
func captureScreenshot(ctx context.Context, u *url.URL, test *testcase) ([]byte, error) {
	var buf []byte
	ctx, cancel := chromedp.NewContext(ctx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tasks := chromedp.Tasks{
		chromedp.EmulateViewport(int64(test.viewportWidth), int64(test.viewportHeight)),
		chromedp.Navigate(u.String()),
		waitForEvent("networkIdle"),
		test.tasks,
	}
	switch test.screenshotType {
	case fullScreenshot:
		tasks = append(tasks, chromedp.FullScreenshot(&buf, 100))
	case viewportScreenshot:
		tasks = append(tasks, chromedp.CaptureScreenshot(&buf))
	case elementScreenshot:
		tasks = append(tasks, chromedp.Screenshot(test.screenshotElement, &buf))
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return nil, fmt.Errorf("chromedp.Run(...): %w", err)
	}
	return buf, nil
}

// writePNG writes image data to a png file.
func writePNG(i *image.Image, filename string) error {
	f, err := os.Create(filename + ".png")
	if err != nil {
		return fmt.Errorf("os.Create(%q): %w", filename+".png", err)
	}
	err = png.Encode(f, *i)
	if err != nil {
		// Ignore f.Close() error, since png.Encode returned an error.
		_ = f.Close()
		return fmt.Errorf("png.Encode(...): %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("f.Close(): %w", err)
	}
	return nil
}

// sanitized transforms text into a string suitable for use in a
// filename part.
func sanitized(text string) string {
	return sanitize.ReplaceAllString(text, "-")
}

// waitForEvent waits for browser lifecycle events. This is useful for
// ensuring the page is fully loaded before capturing screenshots.
func waitForEvent(eventName string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		ch := make(chan struct{})
		cctx, cancel := context.WithCancel(ctx)
		chromedp.ListenTarget(cctx, func(ev interface{}) {
			switch e := ev.(type) {
			case *page.EventLifecycleEvent:
				if e.Name == eventName {
					cancel()
					close(ch)
				}
			}
		})
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
