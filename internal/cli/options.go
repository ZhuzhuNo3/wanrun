package cli

import (
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
)

const (
	defaultMeasureDuration = 3 * time.Second
)

type Result interface{ isCLIResult() }

type HelpPage uint8

const (
	RootHelpPage HelpPage = iota + 1
	ListHelpPage
	RunHelpPage
)

type Help struct{ page HelpPage }

type Version struct{}

type List struct {
	request listnetworks.Request
}

type Run struct {
	request runcommand.Request
}

func (Help) isCLIResult()    {}
func (Version) isCLIResult() {}
func (List) isCLIResult()    {}
func (Run) isCLIResult()     {}

func (result Help) Page() HelpPage { return result.page }
func (result Help) Text() string   { return result.page.Text() }

func (result List) Request() listnetworks.Request { return result.request }

func (result Run) Request() runcommand.Request { return result.request }

type listOptions struct {
	networks      []string
	all           bool
	probe         bool
	probeURL      string
	probeURLSet   bool
	measure       bool
	measureURL    string
	measureURLSet bool
	duration      string
	durationSet   bool
}

type runOptions struct {
	source         string
	sourceSet      bool
	networks       []string
	auto           bool
	measureURL     string
	measureURLSet  bool
	duration       string
	durationSet    bool
	dns            []string
	logDir         string
	logDirSet      bool
	noTUI          bool
	mouse          bool
	followSymlinks bool
	argv           []string
}
