package core

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

const codexVerificationURL = "https://auth.openai.com/codex/device"

var (
	ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	userCodePattern   = regexp.MustCompile(`\b[A-Z0-9]{3,}(?:-[A-Z0-9]{3,})+\b`)
)

type outputResult struct {
	found       bool
	unavailable bool
	expired     bool
}

func scanBrowserLoginOutput(reader io.Reader, publish func(string)) outputResult {
	scanner := bufio.NewScanner(reader)
	found := false
	for scanner.Scan() {
		if found {
			continue
		}
		if u := urlPattern.FindString(scanner.Text()); u != "" {
			found = true
			publish(strings.TrimRight(u, ".,;)"))
		}
	}
	return outputResult{found: found}
}

func scanCodexDeviceOutput(reader io.Reader, publish func(string, string)) outputResult {
	// ReadString has no token-size ceiling, and this loop keeps consuming after
	// publication so the merged child stream is drained through EOF.
	buffered := bufio.NewReader(reader)
	verificationURL := ""
	userCode := ""
	result := outputResult{}
	for {
		line, err := buffered.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		line = ansiEscapePattern.ReplaceAllString(line, "")
		codeInput := ""
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "device auth timed out"):
			result.expired = true
		case strings.Contains(lower, "device code login is not enabled"),
			strings.Contains(lower, "please contact your workspace admin to enable device code authentication"),
			strings.Contains(lower, "device code request failed"),
			strings.Contains(lower, "unexpected argument '--device-auth'"),
			strings.Contains(lower, "unrecognized option '--device-auth'"):
			result.unavailable = true
		}
		if verificationURL == "" {
			if location := urlPattern.FindStringIndex(line); location != nil {
				u := strings.TrimRight(line[location[0]:location[1]], ".,;)")
				if u == codexVerificationURL {
					verificationURL = codexVerificationURL
					codeInput = line[location[1]:]
				}
			}
		} else {
			codeInput = line
		}
		if userCode == "" && codeInput != "" {
			userCode = userCodePattern.FindString(codeInput)
		}
		if !result.found && verificationURL != "" && userCode != "" {
			result.found = true
			publish(verificationURL, userCode)
		}
		if err != nil {
			break
		}
	}
	return result
}
