package cli

import "strings"

// Parse recognizes exactly the public list and run subcommands.
func Parse(argv []string) (Result, error) {
	if len(argv) == 1 && (argv[0] == "-h" || argv[0] == "--help") {
		return Help{page: RootHelpPage}, nil
	}
	if len(argv) == 1 && argv[0] == "--version" {
		return Version{}, nil
	}
	if len(argv) == 0 {
		return nil, usageErrorf("a subcommand is required (list or run)")
	}
	switch argv[0] {
	case "list":
		return parseList(argv[1:])
	case "run":
		return parseRun(argv[1:])
	default:
		return nil, usageErrorf("unknown subcommand %s (expected list or run)", argv[0])
	}
}

func parseList(argv []string) (Result, error) {
	var options listOptions
	for index := 0; index < len(argv); index++ {
		name := argv[index]
		switch name {
		case "-h", "--help":
			return Help{page: ListHelpPage}, nil
		case "--all":
			if options.all {
				return nil, duplicateOption(name)
			}
			options.all = true
		case "--probe":
			if options.probe {
				return nil, duplicateOption(name)
			}
			options.probe = true
		case "--measure":
			if options.measure {
				return nil, duplicateOption(name)
			}
			options.measure = true
		case "--network":
			value, next, err := optionValue(argv, index, name)
			if err != nil {
				return nil, err
			}
			options.networks, index = append(options.networks, value), next
		case "--probe-url":
			value, next, err := optionValue(argv, index, name)
			if err != nil {
				return nil, err
			}
			if options.probeURLSet {
				return nil, duplicateOption(name)
			}
			options.probeURL, options.probeURLSet, index = value, true, next
		case "--measure-url":
			value, next, err := optionValue(argv, index, name)
			if err != nil {
				return nil, err
			}
			if options.measureURLSet {
				return nil, duplicateOption(name)
			}
			options.measureURL, options.measureURLSet, index = value, true, next
		case "--measure-duration":
			value, next, err := optionValue(argv, index, name)
			if err != nil {
				return nil, err
			}
			if options.durationSet {
				return nil, duplicateOption(name)
			}
			options.duration, options.durationSet, index = value, true, next
		default:
			return nil, usageErrorf("unknown list option %s", name)
		}
	}
	return buildList(options)
}

func parseRun(argv []string) (Result, error) {
	optionArgs, child, help, err := splitRunCommand(argv)
	if err != nil {
		return nil, err
	}
	if help {
		return Help{page: RunHelpPage}, nil
	}
	options := runOptions{argv: child}
	for index := 0; index < len(optionArgs); index++ {
		name := optionArgs[index]
		switch name {
		case "--auto-weight":
			if options.auto {
				return nil, duplicateOption(name)
			}
			options.auto = true
		case "--no-tui":
			if options.noTUI {
				return nil, duplicateOption(name)
			}
			options.noTUI = true
		case "--mouse":
			if options.mouse {
				return nil, duplicateOption(name)
			}
			options.mouse = true
		case "--follow-symlinks":
			if options.followSymlinks {
				return nil, duplicateOption(name)
			}
			options.followSymlinks = true
		case "--source", "--network", "--measure-url", "--measure-duration", "--dns", "--log-dir":
			value, next, valueErr := optionValue(optionArgs, index, name)
			if valueErr != nil {
				return nil, valueErr
			}
			index = next
			switch name {
			case "--source":
				if options.sourceSet {
					return nil, duplicateOption(name)
				}
				options.source, options.sourceSet = value, true
			case "--network":
				options.networks = append(options.networks, value)
			case "--measure-url":
				if options.measureURLSet {
					return nil, duplicateOption(name)
				}
				options.measureURL, options.measureURLSet = value, true
			case "--measure-duration":
				if options.durationSet {
					return nil, duplicateOption(name)
				}
				options.duration, options.durationSet = value, true
			case "--dns":
				options.dns = append(options.dns, value)
			case "--log-dir":
				if options.logDirSet {
					return nil, duplicateOption(name)
				}
				options.logDir, options.logDirSet = value, true
			}
		default:
			return nil, usageErrorf("unknown run option %s", name)
		}
	}
	return buildRun(options)
}

func splitRunCommand(argv []string) ([]string, []string, bool, error) {
	for index := 0; index < len(argv); {
		argument := argv[index]
		if argument == "--" {
			return cloneArgs(argv[:index]), cloneArgs(argv[index+1:]), false, nil
		}
		if !strings.HasPrefix(argument, "-") {
			return cloneArgs(argv[:index]), cloneArgs(argv[index:]), false, nil
		}
		if argument == "-h" || argument == "--help" {
			return nil, nil, true, nil
		}
		arity, known := runOptionArity(argument)
		if !known {
			return nil, nil, false, usageErrorf("unknown run option %s", argument)
		}
		if arity == 1 && (index+1 >= len(argv) || argv[index+1] == "--") {
			return nil, nil, false, usageErrorf("option %s requires a value", argument)
		}
		index += arity + 1
	}
	return cloneArgs(argv), nil, false, nil
}

func runOptionArity(name string) (int, bool) {
	switch name {
	case "--auto-weight", "--no-tui", "--mouse", "--follow-symlinks":
		return 0, true
	case "--source", "--network", "--measure-url", "--measure-duration", "--dns", "--log-dir":
		return 1, true
	default:
		return 0, false
	}
}

func optionValue(argv []string, index int, name string) (string, int, error) {
	if index+1 >= len(argv) || argv[index+1] == "--" {
		return "", index, usageErrorf("option %s requires a value", name)
	}
	return argv[index+1], index + 1, nil
}

func duplicateOption(name string) error {
	return usageErrorf("option %s may be specified only once", name)
}

func cloneArgs(values []string) []string { return append([]string(nil), values...) }
