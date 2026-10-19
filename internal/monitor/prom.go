package monitor

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the Prometheus text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Metrics collects metric families and writes them in the Prometheus text
// exposition format.
type Metrics struct {
	fams []*Family
}

// Family is a metric with its samples.
type Family struct {
	Name, Help, Type string
	samples          []sample
}

type sample struct {
	labels []string // name, value, name, value...
	value  float64
}

func (m *Metrics) family(name, typ, help string) *Family {
	f := &Family{Name: name, Help: help, Type: typ}
	m.fams = append(m.fams, f)
	return f
}

func (m *Metrics) Gauge(name, help string) *Family   { return m.family(name, "gauge", help) }
func (m *Metrics) Counter(name, help string) *Family { return m.family(name, "counter", help) }

// Add records a sample; labels are name/value pairs.
func (f *Family) Add(v float64, labels ...string) {
	f.samples = append(f.samples, sample{labels: labels, value: v})
}

// Bool records 1 or 0.
func (f *Family) Bool(b bool, labels ...string) {
	v := 0.0
	if b {
		v = 1
	}
	f.Add(v, labels...)
}

var (
	helpEsc  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEsc = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Write writes the families that have samples.
func (m *Metrics) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	for _, f := range m.fams {
		if len(f.samples) == 0 {
			continue
		}
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s %s\n", f.Name, helpEsc.Replace(f.Help), f.Name, f.Type)
		for _, s := range f.samples {
			bw.WriteString(f.Name)
			if len(s.labels) > 0 {
				bw.WriteByte('{')
				for i := 0; i+1 < len(s.labels); i += 2 {
					if i > 0 {
						bw.WriteByte(',')
					}
					fmt.Fprintf(bw, `%s="%s"`, s.labels[i], labelEsc.Replace(s.labels[i+1]))
				}
				bw.WriteByte('}')
			}
			bw.WriteByte(' ')
			bw.WriteString(formatValue(s.value))
			bw.WriteByte('\n')
		}
	}
	return bw.Flush()
}

var (
	reMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	reLabelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// CheckExposition validates text in the Prometheus text exposition format
// as strictly as Prometheus parses it: HELP and TYPE before the samples of a
// family, the samples of a family together, valid names, quoted and escaped
// label values, no repeated label or series, numeric values, a final newline.
// It returns the number of samples.
func CheckExposition(text string) (int, error) {
	if text != "" && !strings.HasSuffix(text, "\n") {
		return 0, fmt.Errorf("no newline at the end")
	}
	types := map[string]string{}
	helps := map[string]bool{}
	done := map[string]bool{} // families whose samples ended
	series := map[string]bool{}
	current := ""
	n := 0
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		ln := i + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			f := strings.SplitN(line, " ", 4)
			if len(f) < 3 || (f[1] != "HELP" && f[1] != "TYPE") {
				continue // a comment
			}
			name := f[2]
			if !reMetricName.MatchString(name) {
				return n, fmt.Errorf("line %d: invalid metric name %q", ln, name)
			}
			if done[name] || (current == name && f[1] == "TYPE") {
				return n, fmt.Errorf("line %d: %s of %s after its samples", ln, f[1], name)
			}
			if f[1] == "TYPE" {
				if len(f) != 4 {
					return n, fmt.Errorf("line %d: TYPE without a type", ln)
				}
				switch f[3] {
				case "counter", "gauge", "histogram", "summary", "untyped":
				default:
					return n, fmt.Errorf("line %d: unknown type %q", ln, f[3])
				}
				if _, dup := types[name]; dup {
					return n, fmt.Errorf("line %d: second TYPE for %s", ln, name)
				}
				types[name] = f[3]
			} else {
				if helps[name] {
					return n, fmt.Errorf("line %d: second HELP for %s", ln, name)
				}
				helps[name] = true
			}
			continue
		}
		name, labels, rest, err := parseSample(line)
		if err != nil {
			return n, fmt.Errorf("line %d: %v", ln, err)
		}
		if name != current {
			if current != "" {
				done[current] = true
			}
			if done[name] {
				return n, fmt.Errorf("line %d: samples of %s are not together", ln, name)
			}
			current = name
		}
		if t := types[name]; t == "counter" && !strings.HasSuffix(name, "_total") {
			return n, fmt.Errorf("line %d: counter %s does not end in _total", ln, name)
		}
		key := name + "{" + strings.Join(labels, ",") + "}"
		if series[key] {
			return n, fmt.Errorf("line %d: repeated series %s", ln, key)
		}
		series[key] = true
		fields := strings.Fields(rest)
		if len(fields) == 0 || len(fields) > 2 {
			return n, fmt.Errorf("line %d: expected a value (and a timestamp)", ln)
		}
		switch fields[0] {
		case "+Inf", "-Inf", "NaN":
		default:
			if _, err := strconv.ParseFloat(fields[0], 64); err != nil {
				return n, fmt.Errorf("line %d: invalid value %q", ln, fields[0])
			}
		}
		if len(fields) == 2 {
			if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
				return n, fmt.Errorf("line %d: invalid timestamp %q", ln, fields[1])
			}
		}
		n++
	}
	return n, nil
}

// parseSample splits `name{a="x",b="y"} value` into the name, the sorted
// label pairs and the rest of the line.
func parseSample(line string) (string, []string, string, error) {
	end := strings.IndexAny(line, "{ ")
	if end < 0 {
		return "", nil, "", fmt.Errorf("no value")
	}
	name := line[:end]
	if !reMetricName.MatchString(name) {
		return "", nil, "", fmt.Errorf("invalid metric name %q", name)
	}
	rest := line[end:]
	var labels []string
	if strings.HasPrefix(rest, "{") {
		rest = rest[1:]
		seen := map[string]bool{}
		for {
			rest = strings.TrimLeft(rest, " ")
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return "", nil, "", fmt.Errorf("label without a value")
			}
			ln := strings.TrimSpace(rest[:eq])
			if !reLabelName.MatchString(ln) || strings.HasPrefix(ln, "__") {
				return "", nil, "", fmt.Errorf("invalid label name %q", ln)
			}
			if seen[ln] {
				return "", nil, "", fmt.Errorf("repeated label %q", ln)
			}
			seen[ln] = true
			rest = strings.TrimLeft(rest[eq+1:], " ")
			if !strings.HasPrefix(rest, `"`) {
				return "", nil, "", fmt.Errorf("label %s: value is not quoted", ln)
			}
			var val strings.Builder
			i := 1
			for ; i < len(rest); i++ {
				c := rest[i]
				if c == '\\' {
					if i+1 >= len(rest) {
						return "", nil, "", fmt.Errorf("label %s: bad escape", ln)
					}
					switch rest[i+1] {
					case '\\':
						val.WriteByte('\\')
					case '"':
						val.WriteByte('"')
					case 'n':
						val.WriteByte('\n')
					default:
						return "", nil, "", fmt.Errorf("label %s: bad escape \\%c", ln, rest[i+1])
					}
					i++
					continue
				}
				if c == '"' {
					break
				}
				val.WriteByte(c)
			}
			if i >= len(rest) {
				return "", nil, "", fmt.Errorf("label %s: unterminated value", ln)
			}
			labels = append(labels, ln+"="+strconv.Quote(val.String()))
			rest = strings.TrimLeft(rest[i+1:], " ")
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
			} else if !strings.HasPrefix(rest, "}") {
				return "", nil, "", fmt.Errorf("expected , or } after label %s", ln)
			}
		}
	}
	if !strings.HasPrefix(rest, " ") {
		return "", nil, "", fmt.Errorf("no space before the value")
	}
	sort.Strings(labels)
	return name, labels, rest, nil
}
