package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

func main() {
	expectedPath := flag.String("expected", "", "path to package and top-level test mapping")
	flag.Parse()
	if *expectedPath == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: testresult --expected PATH")
		os.Exit(2)
	}
	expected, err := readExpected(*expectedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test result contract: %v\n", err)
		os.Exit(1)
	}
	if err := checkEvents(expected, bufio.NewScanner(os.Stdin)); err != nil {
		fmt.Fprintf(os.Stderr, "test result contract: %v\n", err)
		os.Exit(1)
	}
}

func readExpected(path string) (map[string]map[string]int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open expected targets: %w", err)
	}
	defer file.Close()
	expected := make(map[string]map[string]int)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 2 || fields[0] == "" || !validTopLevelTest(fields[1]) {
			return nil, fmt.Errorf("invalid expected target %q", scanner.Text())
		}
		if expected[fields[0]] == nil {
			expected[fields[0]] = make(map[string]int)
		}
		if _, duplicate := expected[fields[0]][fields[1]]; duplicate {
			return nil, fmt.Errorf("duplicate expected target %s %s", fields[0], fields[1])
		}
		expected[fields[0]][fields[1]] = 0
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read expected targets: %w", err)
	}
	if len(expected) == 0 {
		return nil, fmt.Errorf("expected target mapping is empty")
	}
	return expected, nil
}

func checkEvents(expected map[string]map[string]int, scanner *bufio.Scanner) error {
	var failures []string
	for scanner.Scan() {
		var event testEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("decode go test event: %w", err)
		}
		if event.Test == "" {
			if event.Action == "fail" {
				failures = append(failures, "package failed: "+event.Package)
			}
			continue
		}
		top := strings.SplitN(event.Test, "/", 2)[0]
		packageTargets, packageExpected := expected[event.Package]
		_, targetExpected := packageTargets[top]
		if !packageExpected || !targetExpected {
			if event.Action == "pass" || event.Action == "fail" || event.Action == "skip" {
				failures = append(failures, "unexpected selected target: "+event.Package+" "+event.Test)
			}
			continue
		}
		if event.Action == "skip" || event.Action == "fail" {
			failures = append(failures, event.Action+": "+event.Package+" "+event.Test)
		}
		if event.Action == "pass" && event.Test == top {
			packageTargets[top]++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read go test events: %w", err)
	}
	for pkg, targets := range expected {
		for target, passes := range targets {
			if passes != 1 {
				failures = append(failures, fmt.Sprintf("%s %s pass count=%d, want 1", pkg, target, passes))
			}
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func validTopLevelTest(name string) bool {
	if !strings.HasPrefix(name, "Test") || len(name) == len("Test") {
		return false
	}
	for _, char := range name {
		if char != '_' && (char < '0' || char > '9') && (char < 'A' || char > 'Z') &&
			(char < 'a' || char > 'z') {
			return false
		}
	}
	return true
}
