package main

import (
	"io"
	"os"

	"github.com/ZhuzhuNo3/transferlanes/internal/buildinfo"
	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/cli"
	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transferworker"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if handled, code := dispatchHidden(argv); handled {
		return code
	}
	return cli.Execute(argv, stdin, stdout, stderr, listnetworks.New(), runcommand.New(), buildinfo.Text)
}

func dispatchHidden(argv []string) (bool, int) {
	if handled, code := runsupervisor.MaybeRun(argv, transferworker.Run); handled {
		return true, code
	}
	if handled, code := childprocesses.MaybeRunHelper(argv); handled {
		return true, code
	}
	return throughput.MaybeRunHelper(argv)
}
