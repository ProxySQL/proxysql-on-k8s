/*
Copyright 2026 ProxySQL.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package builders

import (
	"regexp"
	"strings"
)

// cnfSectionStart matches the opening line of a variables section, e.g.
// "mysql_variables=". The section body is the following "{ ... }" block.
var cnfSectionStart = regexp.MustCompile(`^(admin|mysql|pgsql)_variables=$`)

// cnfVarLine matches a single "key=value" line inside a variables section.
// Values may be double-quoted strings or bare tokens (ints/bools); the raw
// value text (including quotes, if any) is captured and unquoted by the
// caller.
var cnfVarLine = regexp.MustCompile(`^(\s*)([a-z0-9_]+)=(.*?)\s*$`)

// cnfLine is one line of a rendered cnf, classified during the shared scan
// that backs both ParseCnfVariables and NormalizeCnf.
type cnfLine struct {
	text     string // original line text, verbatim
	prefix   string // text before "key=" (leading whitespace), set only for variable lines
	key      string // bare key (no section prefix), set only for variable lines
	fullName string // sectionPrefix + "-" + key, set only for variable lines
	value    string // raw captured value (quotes intact), set only for variable lines
	isVar    bool   // true if this line is a "key=value" variable line
}

// scanCnf walks a rendered cnf line by line, tracking (admin|mysql|pgsql)
// variable sections and classifying each line. Section boundaries and
// everything outside them (datadir, proxysql_servers, comments, blank
// lines, the closing "}") are treated as structural.
func scanCnf(cnf string) []cnfLine {
	lines := strings.Split(cnf, "\n")
	out := make([]cnfLine, 0, len(lines))

	inSection := false
	prefix := ""
	for _, line := range lines {
		if !inSection {
			if m := cnfSectionStart.FindStringSubmatch(line); m != nil {
				inSection = true
				prefix = m[1]
			}
			out = append(out, cnfLine{text: line})
			continue
		}

		// Inside a section: "^}" (no leading whitespace) closes it.
		if line == "}" {
			inSection = false
			prefix = ""
			out = append(out, cnfLine{text: line})
			continue
		}

		if m := cnfVarLine.FindStringSubmatch(line); m != nil {
			leading, key, value := m[1], m[2], m[3]
			out = append(out, cnfLine{
				text:     line,
				prefix:   leading,
				key:      key,
				fullName: prefix + "-" + key,
				value:    value,
				isVar:    true,
			})
			continue
		}

		out = append(out, cnfLine{text: line})
	}
	return out
}

// unquote strips a single pair of surrounding double quotes, if present.
func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// ParseCnfVariables returns runtime-appliable variables from a rendered cnf,
// keyed by FULL variable name (admin-*, mysql-*, pgsql-*). Reserved keys
// (reservedCnfKeys) are excluded. Values are unquoted so they can be
// compared directly against runtime_global_variables values.
func ParseCnfVariables(cnf string) map[string]string {
	out := map[string]string{}
	for _, l := range scanCnf(cnf) {
		if !l.isVar {
			continue
		}
		if _, reserved := reservedCnfKeys[l.fullName]; reserved {
			continue
		}
		out[l.fullName] = unquote(l.value)
	}
	return out
}

// NormalizeCnf replaces every runtime-appliable variable VALUE with the
// fixed placeholder "<runtime>" and returns the result. Reserved keys and
// all structural text are left verbatim, so:
//
//	NormalizeCnf(a) == NormalizeCnf(b)  <=>  a and b differ only in
//	runtime-appliable variable values (same key set, same structure).
func NormalizeCnf(cnf string) string {
	lines := scanCnf(cnf)
	out := make([]string, len(lines))
	for i, l := range lines {
		if !l.isVar {
			out[i] = l.text
			continue
		}
		if _, reserved := reservedCnfKeys[l.fullName]; reserved {
			out[i] = l.text
			continue
		}
		// Preserve the original line's leading whitespace and bare key,
		// replacing only the value.
		out[i] = l.prefix + l.key + "=<runtime>"
	}
	return strings.Join(out, "\n")
}
