package onhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The self-check's contract.
const (
	stagePass = "pass"
	stageFail = "fail"
	// selfcheckMaxSeconds is systemd's hard limit on one stage; the binary
	// keeps its own budget of 30 seconds inside it.
	selfcheckMaxSeconds = 45
	// selfcheckMaxReport is how much of the report is read.
	selfcheckMaxReport = 64 << 10
)

// checkLine is one line of the report.
type checkLine struct {
	App    string `json:"app"`
	Check  string `json:"check"`
	Stage  string `json:"stage"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	MS     int64  `json:"ms"`
}

var checkName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// selfcheck runs one stage of the release's selfcheck in the service's
// sandbox and returns pass or fail with the names of the checks that
// failed. Details go to the operator's log only.
func (h *Host) selfcheck(ctx context.Context, r Request, bin, cfg, envFile, stage string) (string, []string) {
	ctx, cancel := context.WithTimeout(ctx, (selfcheckMaxSeconds+15)*time.Second)
	defer cancel()
	h.logf("self-check, %s stage", stage)
	out := &capped{max: selfcheckMaxReport}
	status, err := h.Sys.RunSandboxed(ctx, r.Target.User, []string{envFile}, r.Selfcheck.Properties, selfcheckMaxSeconds,
		[]string{bin, "selfcheck", "-c", cfg, "--stage", stage}, out, h.Log)
	if err != nil {
		h.logf("  the self-check could not run: %v", err)
		return stageFail, []string{"selfcheck/" + stage}
	}
	var failed []string
	passed := 0
	sc := bufio.NewScanner(bytes.NewReader(out.buf.Bytes()))
	sc.Buffer(make([]byte, 0, 4096), selfcheckMaxReport)
	for sc.Scan() {
		var l checkLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue // not a report line; the binary's logs go to standard error
		}
		switch l.Status {
		case "pass":
			passed++
		case "fail":
			name := l.App + "/" + l.Check
			if !checkName.MatchString(l.App) || !checkName.MatchString(l.Check) {
				name = "selfcheck/unnamed"
			}
			failed = append(failed, name)
			h.logf("  FAIL %s: %s", name, oneLine(l.Detail))
		}
	}
	if out.truncated {
		h.logf("  the report passed %d KiB and was cut there", selfcheckMaxReport>>10)
		failed = append(failed, "selfcheck/report-too-long")
	}
	switch {
	case status == 0 && len(failed) == 0:
		h.logf("  %d checks passed", passed)
		return stagePass, nil
	case status == 1 && len(failed) == 0:
		failed = append(failed, "selfcheck/"+stage)
	case status != 0 && status != 1:
		h.logf("  the self-check could not run (exit status %d)", status)
		failed = append(failed, "selfcheck/"+stage)
	}
	return stageFail, failed
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// capped keeps the first max bytes written to it.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// RunSandboxed implements System with systemd-run, as RunAs does, adding
// the service's sandbox and RuntimeMaxSec.
func (Systemd) RunSandboxed(ctx context.Context, usr string, envFiles, props []string, maxSeconds int, argv []string, stdout, stderr io.Writer) (int, error) {
	args := []string{"--quiet", "--wait", "--pipe", "--collect", "--uid=" + usr, "--gid=" + usr,
		"-p", "RuntimeMaxSec=" + strconv.Itoa(maxSeconds)}
	for _, f := range envFiles {
		args = append(args, "-p", "EnvironmentFile="+f)
	}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	args = append(args, "--")
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "systemd-run", args...) //nolint:gosec // G204: the binary, paths and properties come from a validated request
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("onhost: systemd-run: %w", err)
	}
	return 0, nil
}
