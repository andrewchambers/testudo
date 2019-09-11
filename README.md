# testudo

A simple and non opinionated parallel test runner.

## Features

- Language agnostic.
- Parallel test runs without jumbling output.
- Streaming test output, not held in memory longer than needed.
- Ctrl+C Cancellation.

## Usage

```
testudo [flags] [test-sources...]

  testudo is a parallel test runner, executing and aggregating results from test scripts.

  Valid flags: 
  -f, --format string   Output format, valid values: human,jsonl. (default "human")
  -p, --parallel int    Execute this many tests in parallel. (default 1)

  A test source is one of:
    - An executable script emitting test csv.
    - A directory containing a script called 'collect.tests' which emits test csv.

  Test csv consists of lines matching:

    - A 3 tuple specifying 'test', a test runner script and the test case.
      e.g. test,$PATH_TO_RUNNER,$PATH_TO_TEST

  If no test sources are specified then './collect.tests' and './tests' are tried.
```

## Examples

```
$ testsudo
+++ fail.c
+ cc /home/ac/src/testudo/tests/fail.c -o /home/ac/src/testudo/tests/fail.c.bin
+ /home/ac/src/testudo/tests/fail.c.bin
--- failed... exit status 3
+++ pass.c
+ cc /home/ac/src/testudo/tests/pass.c -o /home/ac/src/testudo/tests/pass.c.bin
+ /home/ac/src/testudo/tests/pass.c.bin

passed 1/2
```

```
$ testudo --parallel 4 --format jsonl 2> /dev/null
{"Name":"pass.c","Path":"/home/ac/src/testudo/tests/pass.c","Runner":"/home/ac/src/testudo/tests/default.runner","Err":"","Passed":true,"Duration":72459896,"Output":"..."}
{"Name":"fail.c","Path":"/home/ac/src/testudo/tests/fail.c","Runner":"/home/ac/src/testudo/tests/default.runner","Err":"exit status 3","Passed":false,"Duration":72525785,"Output":"...\n"}
```

## Tips

- Write your test runners scripts such that they work from any directory and you
  will be able to run individual tests by hand easily when debugging individual tests.