// Command rulecheck runs Tailwise's heuristic rules over the benchmark's
// requests without scoring them, so a rule set is measured on cached Jev
// answers at no cost. It reads JSON lines {"id": ..., "request": <the OTLP
// JSON request the benchmark sends>} on stdin and writes {"id": ...,
// "rules": [{"name", "keep", "fired", "reason"}]} for each, in order.
//
//	go run ./bench/rulecheck -rules bench/rules/tau.yaml < requests.jsonl
//
// The trace is assembled exactly as POST /v1/otlp/traces:score does.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"gopkg.in/yaml.v3"

	"github.com/tecnosam/tailwise/internal/otlp"
	"github.com/tecnosam/tailwise/internal/rules"
)

type input struct {
	ID      string          `json:"id"`
	Request json.RawMessage `json:"request"`
}

type verdict struct {
	Name   string `json:"name"`
	Keep   bool   `json:"keep"`
	Fired  bool   `json:"fired"`
	Reason string `json:"reason,omitempty"`
}

func main() {
	path := flag.String("rules", "", "a YAML file with a rules: list, as in the Tailwise config")
	flag.Parse()
	if err := run(*path); err != nil {
		fmt.Fprintln(os.Stderr, "rulecheck:", err)
		os.Exit(1)
	}
}

func run(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file struct {
		Rules []rules.Config `yaml:"rules"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	set, err := rules.New(file.Rules)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if set == nil {
		return fmt.Errorf("%s: no rules", path)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 1<<30)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	assembler := otlp.Assembler{}
	for in.Scan() {
		var line input
		if err := json.Unmarshal(in.Bytes(), &line); err != nil {
			return err
		}
		var req coltracepb.ExportTraceServiceRequest
		if err := otlp.UnmarshalJSON(line.Request, &req); err != nil {
			return fmt.Errorf("%s: %w", line.ID, err)
		}
		byTrace, _, err := otlp.Split(req.GetResourceSpans())
		if err != nil {
			return fmt.Errorf("%s: %w", line.ID, err)
		}
		if len(byTrace) != 1 {
			return fmt.Errorf("%s: %d traces in one request", line.ID, len(byTrace))
		}
		var verdicts []verdict
		for id, spans := range byTrace {
			t, err := assembler.Assemble(id, [][]byte{spans})
			if err != nil {
				return fmt.Errorf("%s: %w", line.ID, err)
			}
			for _, r := range set.Check(t) {
				verdicts = append(verdicts, verdict{Name: r.Name, Keep: r.Keep, Fired: r.Fired, Reason: r.Reason})
			}
		}
		if err := enc.Encode(map[string]any{"id": line.ID, "rules": verdicts}); err != nil {
			return err
		}
	}
	if err := in.Err(); err != nil {
		return errors.Join(errors.New("reading stdin"), err)
	}
	return nil
}
