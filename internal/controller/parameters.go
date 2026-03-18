package controller

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// templateFuncs provides Sprig-compatible integer math functions for PG parameter templates.
var templateFuncs = template.FuncMap{
	"div": func(a, b int64) int64 { return a / b },
	"mul": func(a, b int64) int64 { return a * b },
	"add": func(a, b int64) int64 { return a + b },
	"max": func(a, b int64) int64 {
		if a > b {
			return a
		}
		return b
	},
}

// parameterData is the data passed to PG parameter templates.
type parameterData struct {
	// Memory in bytes.
	Memory int64 `json:"memory"`
	// CPU in cores.
	CPU int64 `json:"cpu"`
}

// CalculatePostgresParameters evaluates PG parameter templates with the given memory and CPU values.
// Templates starting with "{{" are evaluated as Go templates; other values pass through unchanged.
// Memory is in bytes, CPU is in whole cores — matching the helm chart convention.
func CalculatePostgresParameters(params map[string]string, memoryBytes int64, cpuCores int64) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}

	data := parameterData{Memory: memoryBytes, CPU: cpuCores}
	result := make(map[string]string, len(params))

	for name, tmplStr := range params {
		val, err := evaluateParameter(name, tmplStr, data)
		if err != nil {
			return nil, fmt.Errorf("evaluating parameter %q: %w", name, err)
		}
		result[name] = val
	}

	return result, nil
}

// evaluateParameter evaluates a single parameter template string.
// If the value starts with "{{", it is treated as a Go template; otherwise it is returned as-is.
func evaluateParameter(name, tmplStr string, data parameterData) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(tmplStr), "{{") {
		return tmplStr, nil
	}

	// Use .memory and .cpu (lowercase) to match helm chart templates.
	// We wrap the data in a map so template expressions use {{ .memory }} not {{ .Memory }}.
	tmplData := map[string]int64{
		"memory": data.Memory,
		"cpu":    data.CPU,
	}

	t, err := template.New(name).Funcs(templateFuncs).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, tmplData); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return strings.TrimSpace(buf.String()), nil
}
