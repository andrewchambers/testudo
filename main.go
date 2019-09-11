package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	flag "github.com/spf13/pflag"
)

var (
	bufferOutput = true

	outFormat = flag.StringP("format", "f", "human", "Output format, valid values: human,jsonl.")
	parallel  = flag.IntP("parallel", "p", 1, "Execute this many tests in parallel.")
)

type TestCase struct {
	Name     string
	Path     string
	Runner   string
	Err      string
	Passed   bool
	Duration time.Duration
	Output   string

	// Only used when bufferOutput is true
	outputBufferPath string
}

func die(msg string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, msg, args...)
	os.Exit(1)
}

func runCmdContext(ctx context.Context, cmd *exec.Cmd) error {
	err := cmd.Start()
	if err != nil {
		return err
	}

	childExited := make(chan struct{})
	childWg := &sync.WaitGroup{}
	childWg.Add(1)

	go func() {
		defer childWg.Done()
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(unix.SIGTERM)
		case <-childExited:
		}

		<-childExited
	}()

	err = cmd.Wait()
	close(childExited)
	childWg.Wait()

	return err
}

func logTestHeader(t *TestCase) {
	_, _ = fmt.Fprintf(os.Stderr, "+++ %s\n", t.Name)
}

func logTestFooter(t *TestCase) {
	if !t.Passed {
		_, _ = fmt.Fprintf(os.Stderr, "--- failed... %s\n", t.Err)
	}
}

func collectTests(ctx context.Context, testSource string) ([]*TestCase, error) {
	testSource, err := filepath.Abs(testSource)
	if err != nil {
		return nil, err
	}

	st, err := os.Stat(testSource)
	if err != nil {
		return nil, err
	}

	var collector string
	var testDir string

	if st.IsDir() {
		testDir = testSource
		collector = filepath.Join(testDir, "collect.tests")
	} else {
		testDir = filepath.Dir(testSource)
		collector = testSource
	}

	var testsBuffer bytes.Buffer

	cmd := exec.Command(collector)

	cmd.Dir = testDir
	cmd.Stdout = &testsBuffer
	cmd.Stderr = os.Stderr

	err = runCmdContext(ctx, cmd)
	if err != nil {
		return nil, errors.Wrapf(err, "error running test collector '%s'", collector)
	}

	records, err := csv.NewReader(&testsBuffer).ReadAll()
	if err != nil {
		return nil, err
	}

	var tests []*TestCase

	for _, rec := range records {
		switch rec[0] {
		case "test":
			if len(rec) != 3 {
				return nil, errors.Errorf("expected 3 values for a test during test collection: got %#v", rec)
			}
			runnerPath := rec[1]
			testPath := rec[2]

			if !filepath.IsAbs(testPath) {
				return nil, errors.Errorf("test collector must output absolute paths: got %#v", rec)
			}

			if !strings.HasPrefix(testPath, testDir) || !(len(testPath) > len(testDir)) {
				return nil, errors.Errorf("test collector returned test %s that is under the test dir %s", strconv.Quote(testPath), strconv.Quote(testDir))
			}

			testName := testPath[len(testDir)+1:]

			tests = append(tests, &TestCase{
				Path:   testPath,
				Name:   testName,
				Runner: runnerPath,
			})
		default:
			return nil, errors.Errorf("unknown record type: %s", rec[0])
		}
	}

	return tests, nil
}

func runTests(ctx context.Context, tmpDir string, tests []*TestCase) error {

	for i, test := range tests {
		test.outputBufferPath = filepath.Join(fmt.Sprintf("test-%d.log", i))
	}

	wg := &sync.WaitGroup{}
	testChan := make(chan *TestCase)
	testCompleteChan := make(chan *TestCase)

	testWorker := func(ctx context.Context) {
		defer wg.Done()

		for {
			select {
			case t, ok := <-testChan:
				func() {
					if !ok {
						return
					}

					var cmd *exec.Cmd

					if t.Runner == "" {
						cmd = exec.Command(t.Path)
					} else {
						cmd = exec.Command(t.Runner, t.Path)
					}
					cmd.Dir = filepath.Dir(t.Runner)

					if !bufferOutput {
						logTestHeader(t)
						cmd.Stdout = os.Stderr
						cmd.Stderr = os.Stderr
					} else {
						f, err := os.Create(t.outputBufferPath)
						if err != nil {
							t.Err = err.Error()
							select {
							case testCompleteChan <- t:
							case <-ctx.Done():
							}
							return
						}
						defer f.Close()
						cmd.Stdout = f
						cmd.Stderr = f
					}

					startt := time.Now()
					err := runCmdContext(ctx, cmd)
					endt := time.Now()
					t.Duration = endt.Sub(startt)

					if err != nil {
						t.Err = err.Error()
					} else {
						t.Passed = true
					}

					if !bufferOutput {
						logTestFooter(t)
					}

					select {
					case testCompleteChan <- t:
					case <-ctx.Done():
					}
				}()
			case <-ctx.Done():
				return
			}
		}
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)

	for i := 0; i < *parallel; i++ {
		wg.Add(1)
		go testWorker(workerCtx)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		for _, tc := range tests {
			select {
			case testChan <- tc:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer cancelWorkers()
		defer wg.Done()

		for _, _ = range tests {
			select {
			case t := <-testCompleteChan:
				func() {
					if !bufferOutput {
						return
					}
					logTestHeader(t)
					f, err := os.Open(t.outputBufferPath)
					if err != nil {
						return
					}
					defer f.Close()
					defer os.Remove(f.Name())
					switch *outFormat {
					case "human":
						_, _ = io.Copy(os.Stderr, f)
					case "jsonl":
						output, err := ioutil.ReadAll(io.LimitReader(f, 32*1024*1024))
						if err != nil {
							die("%s\n", err)
						}
						t.Output = string(output)
						jsonBytes, err := json.Marshal(t)
						if err != nil {
							die("%s\n", err)
						}
						t.Output = "" // Allow GC of potentially large output.
						_, err = fmt.Fprintf(os.Stdout, "%s\n", string(jsonBytes))
						if err != nil {
							die("%s\n", err)
						}
					default:
						panic(*outFormat)
					}
					logTestFooter(t)
				}()
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()

	err := ctx.Err()
	if err != nil {
		return err
	}

	return nil
}

func main() {
	var err error

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s [flags] [test-sources...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n  %s is a parallel test runner, executing and aggregating results from test scripts.\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n  Valid flags: \n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n  A test source is one of:")
		fmt.Fprintf(os.Stderr, "\n    - An executable script emitting test csv.")
		fmt.Fprintf(os.Stderr, "\n    - A directory containing a script called 'collect.tests' which emits test csv.\n")
		fmt.Fprintf(os.Stderr, "\n  Test csv consists of lines matching:\n")
		fmt.Fprintf(os.Stderr, "\n    - A 3 tuple specifying 'test', a test runner script and the test case.")
		fmt.Fprintf(os.Stderr, "\n      e.g. test,$PATH_TO_RUNNER,$PATH_TO_TEST\n")
		fmt.Fprintf(os.Stderr, "\n  If no test sources are specified then './collect.tests' and './tests' are tried.\n")
		fmt.Fprintln(os.Stderr, "")
	}

	flag.Parse()

	if *parallel < 1 {
		die("--parallel must be greater than or equal to 1\n")
	}

	bufferOutput = !(*parallel == 1 && *outFormat == "human")

	switch *outFormat {
	case "human":
	case "jsonl":
	default:
		die("unsupported output format: %s\n", *outFormat)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		_, _ = fmt.Fprintf(os.Stderr, "Got interrupt, cancelling tests.\n")
		cancel()
		select {
		case <-time.After(10 * time.Second):
			_, _ = fmt.Fprintf(os.Stderr, "Cancel timer expired.\n")
		case <-c:
		}
		die("Aborting.\n")
	}()

	testSources := flag.Args()

	if len(testSources) == 0 {
		for _, c := range []string{"./collect.tests", "./tests"} {
			_, err := os.Stat(c)
			if err == nil {
				testSources = append(testSources, c)
			}
		}
	}

	var allTests []*TestCase

	for _, testSource := range testSources {
		tests, err := collectTests(ctx, testSource)
		if err != nil {
			die("Error collecting tests: %s\n", err)
		}
		allTests = append(allTests, tests...)
	}

	tmpDir, err := ioutil.TempDir("", "")
	if err != nil {
		die("%s\n")
	}
	err = runTests(ctx, tmpDir, allTests)
	_ = os.RemoveAll(tmpDir)
	if err != nil {
		die("Error running tests: %s\n", err)
	}

	passed := 0
	failed := 0

	for _, t := range allTests {
		if t.Passed {
			passed += 1
		} else {
			failed += 1
		}
	}

	_, _ = fmt.Fprintf(os.Stderr, "\npassed %d/%d\n", passed, passed+failed)

	if failed != 0 {
		os.Exit(1)
	}
}
