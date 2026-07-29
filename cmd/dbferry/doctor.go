package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"filippo.io/age"

	"github.com/dbferry/dbferry/config"
	"github.com/dbferry/dbferry/pipeline"
)

// cmdDoctor diagnoses a source (and its destination) before a real backup:
// connect, dump-client compatibility, read access, and destination write/read/
// delete, each with a concrete fix (poc-plan 0.5.5).
func cmdDoctor(args []string, stdout, stderr io.Writer, stdoutTTY, stderrTTY bool) int {
	fs := newFlagSet("doctor", stderr)
	var (
		connName   = fs.String("connection", "", "named connection to diagnose")
		cfgPath    = fs.String("config", "", "config file path")
		dsnEnv     = fs.String("dsn-env", "DBFERRY_DSN", "env var holding the DSN")
		dsnFile    = fs.String("dsn-file", "", "file holding the DSN")
		dest       = fs.String("dest", "", "destination to diagnose (without --connection)")
		s3Endpoint = fs.String("s3-endpoint", "", "S3-compatible endpoint")
		jsonOut    = fs.Bool("json", false, "print the checks as JSON")
		noColor    = fs.Bool("no-color", false, "disable ANSI colour")
	)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	out := &ui{stdout: stdout, stderr: stderr, stdoutTTY: stdoutTTY, stderrTTY: stderrTTY, json: *jsonOut, color: !*noColor}
	if *connName != "" && (*dsnFile != "" || *dest != "" || *s3Endpoint != "" || flagWasSet(fs, "dsn-env")) {
		return out.fail(usageErr("choose either --connection or the standalone --dsn-env/--dsn-file/--dest/--s3-endpoint flags, not both"), redactNothing)
	}

	var (
		red       config.Redactor
		dsn       string
		destCfg   pipeline.Config
		hasDest   bool
		recipient string
		// destBroken carries a destination that could not even be resolved —
		// doctor must report that as a failed check, not silently skip the
		// thing it was asked to diagnose.
		destBroken *pipeline.Check
	)

	if *connName != "" {
		cfg, err := config.Load(configPath(*cfgPath))
		if err != nil {
			return out.fail(err, redactNothing)
		}
		conn := cfg.Connections[*connName]
		if conn == nil {
			return out.fail(usageErr(fmt.Sprintf("no connection named %q", *connName)), redactNothing)
		}
		d, secrets, err := conn.ConnectDSN()
		if err != nil {
			return out.fail(err, red.Redact)
		}
		red.Add(secrets...)
		dsn = d
		recipient = conn.AgeRecipient
		if conn.Destination != "" {
			dst := cfg.Destinations[conn.Destination]
			switch {
			case dst == nil:
				destBroken = &pipeline.Check{Name: "destination", Status: pipeline.StatusFail,
					Detail: fmt.Sprintf("connection references destination %q, which does not exist", conn.Destination),
					Fix:    "add it with `dbferry destinations add` or point the connection at an existing one"}
			default:
				s3, dsecrets, rerr := dst.Resolve()
				if rerr != nil {
					destBroken = &pipeline.Check{Name: "destination", Status: pipeline.StatusFail,
						Detail: fmt.Sprintf("destination %q: %v", conn.Destination, rerr),
						Fix:    "export the referenced credential env vars (or fix the destination entry)"}
				} else {
					red.Add(dsecrets...)
					destCfg = destProbeConfig(dst, s3)
					hasDest = true
				}
			}
		}
	} else {
		d, derr := resolveDSN(*dsnEnv, *dsnFile)
		if derr != nil {
			return out.fail(derr, redactNothing)
		}
		dsn = d
		red.Add(d)
		if pw := passwordOf(d); pw != "" {
			red.Add(pw)
		}
		if *dest != "" {
			destCfg = pipeline.Config{Dest: *dest, S3Endpoint: *s3Endpoint}
			hasDest = true
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	checks := pipeline.DiagnoseSource(ctx, dsn)
	if hasDest {
		checks = append(checks, pipeline.DiagnoseDestination(ctx, destCfg)...)
	}
	if destBroken != nil {
		checks = append(checks, *destBroken)
	}
	if recipient != "" {
		if _, err := age.ParseX25519Recipient(recipient); err != nil {
			checks = append(checks, pipeline.Check{Name: "age recipient", Status: pipeline.StatusFail,
				Detail: err.Error(), Fix: "fix age_recipient on the connection (a valid age1… key)"})
		} else {
			checks = append(checks, pipeline.Check{Name: "age recipient", Status: pipeline.StatusOK, Detail: "valid"})
		}
	}

	return out.renderChecks(checks, red.Redact)
}

// renderChecks prints diagnostic checks and returns 1 if any failed.
func (u *ui) renderChecks(checks []pipeline.Check, redact func(string) string) int {
	failed := false
	for _, c := range checks {
		if c.Status == pipeline.StatusFail {
			failed = true
		}
	}
	if u.json {
		type jc struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail,omitempty"`
			Fix    string `json:"fix,omitempty"`
		}
		var list []jc
		for _, c := range checks {
			list = append(list, jc{c.Name, c.Status.String(), redact(c.Detail), redact(c.Fix)})
		}
		u.writeJSONValue(struct {
			OK     bool `json:"ok"`
			Checks []jc `json:"checks"`
		}{OK: !failed, Checks: list})
		if failed {
			return 1
		}
		return 0
	}
	for _, c := range checks {
		sym, col := "✓", colorGreen
		switch c.Status {
		case pipeline.StatusWarn:
			sym, col = "!", colorYellow
		case pipeline.StatusFail:
			sym, col = "✗", colorRed
		}
		fmt.Fprintf(u.stdout, "%s %-16s %s\n", paint(u.color && u.stdoutTTY, col, sym), c.Name, redact(c.Detail))
		if c.Fix != "" {
			fmt.Fprintf(u.stdout, "    → %s\n", redact(c.Fix))
		}
	}
	if failed {
		return 1
	}
	return 0
}
