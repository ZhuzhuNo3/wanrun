package namespaceresolvers

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	maximumResolverBytes  = 64 * 1024
	maximumNameservers    = 3
	maximumDNSMessage     = 4096
	maximumDNSWireMessage = 65535
)

type resolverConfig struct {
	nameservers []netip.Addr
	suffix      []string
	timeout     time.Duration
	attempts    int
	rotate      bool
	useTCP      bool
}

func parseResolverConfig(content []byte) (resolverConfig, error) {
	if len(content) == 0 || len(content) > maximumResolverBytes {
		return resolverConfig{}, errors.New("host resolver configuration is empty or too large")
	}
	config := resolverConfig{timeout: 5 * time.Second, attempts: 2}
	hasSearchDirective := false
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "nameserver":
			if len(fields) != 2 {
				return resolverConfig{}, errors.New("host resolver contains an invalid nameserver directive")
			}
			address, err := netip.ParseAddr(strings.Trim(fields[1], "[]"))
			if err != nil || !address.IsValid() || address.IsUnspecified() || address.IsMulticast() {
				return resolverConfig{}, fmt.Errorf("host resolver nameserver %q is invalid", fields[1])
			}
			if len(config.nameservers) < maximumNameservers {
				config.nameservers = append(config.nameservers, address)
			}
		case "search", "domain":
			if hasSearchDirective || fields[0] == "domain" && len(fields) != 2 ||
				fields[0] == "search" && (len(fields) < 2 || len(fields) > 7) {
				return resolverConfig{}, errors.New("host resolver search directive is invalid")
			}
			for _, domain := range fields[1:] {
				if !validDomain(domain) {
					return resolverConfig{}, fmt.Errorf("host resolver search domain %q is invalid", domain)
				}
			}
			hasSearchDirective = true
			config.suffix = append(config.suffix, strings.Join(fields, " "))
		case "options":
			childOptions, err := parseResolverOptions(&config, fields[1:])
			if err != nil {
				return resolverConfig{}, err
			}
			if len(childOptions) != 0 {
				config.suffix = append(config.suffix, "options "+strings.Join(childOptions, " "))
			}
		case "sortlist":
			return resolverConfig{}, errors.New("host resolver sortlist is unsupported")
		default:
			return resolverConfig{}, fmt.Errorf("host resolver directive %q is unsupported", fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return resolverConfig{}, fmt.Errorf("read host resolver configuration: %w", err)
	}
	if len(config.nameservers) == 0 {
		return resolverConfig{}, errors.New("host resolver contains no nameserver")
	}
	return config, nil
}

func parseResolverOptions(config *resolverConfig, options []string) ([]string, error) {
	childOptions := make([]string, 0, len(options))
	for _, option := range options {
		name, raw, hasValue := strings.Cut(option, ":")
		switch name {
		case "rotate":
			if hasValue {
				return nil, fmt.Errorf("host resolver option %q is invalid", option)
			}
			config.rotate = true
		case "timeout":
			value, err := boundedResolverOption(raw, hasValue, 1, 30)
			if err != nil {
				return nil, fmt.Errorf("host resolver option %q is invalid", option)
			}
			config.timeout = time.Duration(value) * time.Second
		case "attempts":
			value, err := boundedResolverOption(raw, hasValue, 1, 5)
			if err != nil {
				return nil, fmt.Errorf("host resolver option %q is invalid", option)
			}
			config.attempts = value
		case "ndots":
			if _, err := boundedResolverOption(raw, hasValue, 0, 15); err != nil {
				return nil, fmt.Errorf("host resolver option %q is invalid", option)
			}
			childOptions = append(childOptions, option)
		case "single-request", "single-request-reopen", "no-tld-query", "edns0":
			if hasValue {
				return nil, fmt.Errorf("host resolver option %q is invalid", option)
			}
			childOptions = append(childOptions, option)
		case "use-vc":
			if hasValue {
				return nil, fmt.Errorf("host resolver option %q is invalid", option)
			}
			config.useTCP = true
		default:
			return nil, fmt.Errorf("host resolver option %q is unsupported", option)
		}
	}
	return childOptions, nil
}

func boundedResolverOption(raw string, present bool, minimum, maximum int) (int, error) {
	if !present || raw == "" {
		return 0, errors.New("missing option value")
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, errors.New("option is outside bounds")
	}
	return value, nil
}

func validDomain(domain string) bool {
	if domain == "." {
		return true
	}
	if len(domain) == 0 || len(domain) > 253 || strings.ContainsAny(domain, "\x00/\\") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(domain, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}
