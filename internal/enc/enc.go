// Package enc is the one encoding every record camp writes uses.
//
// The problem it solves is not hypothetical. A Linux name may contain
// spaces, tabs, the sequence " -> ", and bytes that are not valid UTF-8.
// An earlier record format separated fields with a space and an arrow,
// and it was not injective: two different filesystem states could
// serialise to the same line, or produce a line that would not parse
// back. A snapshot that cannot represent what it saw is a snapshot that
// will one day report a change that did not happen, or miss one that did.
//
// So: fields are raw bytes under reversible C-style escaping. A backslash
// becomes \\, TAB \t, LF \n, CR \r, and every other byte below 0x20 and
// 0x7F becomes \xHH. Everything else -- non-UTF-8 included -- passes
// through verbatim. A decoded field can hold anything a Linux name can;
// an encoded field can never hold a raw TAB or newline, so TAB can frame
// the fields and LF can frame the records without ambiguity.
//
// A line that does not decode refuses the whole file. Half-reading a
// snapshot is worse than not reading it.
//
// Sorting compares decoded bytes, never the encoded form and never a
// locale order: a locale sort and a byte sort silently disagree, and the
// gate, the inventory and the exclude all have to describe the same set.
package enc

import (
	"fmt"
	"sort"
	"strings"
)

// Encode renders one field.
func Encode(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character == '\\':
			out.WriteString(`\\`)
		case character == '\t':
			out.WriteString(`\t`)
		case character == '\n':
			out.WriteString(`\n`)
		case character == '\r':
			out.WriteString(`\r`)
		case character < 0x20 || character == 0x7f:
			fmt.Fprintf(&out, `\x%02x`, character)
		default:
			out.WriteByte(character)
		}
	}
	return out.String()
}

// Decode reads one field back, byte for byte.
func Decode(value string) (string, error) {
	var out strings.Builder
	out.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			out.WriteByte(value[index])
			continue
		}
		index++
		if index >= len(value) {
			return "", fmt.Errorf("the field ends with a backslash that opens no escape: %q", value)
		}
		switch value[index] {
		case '\\':
			out.WriteByte('\\')
		case 't':
			out.WriteByte('\t')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 'x':
			if index+2 >= len(value) {
				return "", fmt.Errorf("a \\x escape is cut short in %q", value)
			}
			high, err := hex(value[index+1])
			if err != nil {
				return "", fmt.Errorf("in %q: %w", value, err)
			}
			low, err := hex(value[index+2])
			if err != nil {
				return "", fmt.Errorf("in %q: %w", value, err)
			}
			out.WriteByte(high<<4 | low)
			index += 2
		default:
			return "", fmt.Errorf("%q is not an escape camp writes, in %q",
				`\`+string(value[index]), value)
		}
	}
	return out.String(), nil
}

func hex(character byte) (byte, error) {
	switch {
	case character >= '0' && character <= '9':
		return character - '0', nil
	case character >= 'a' && character <= 'f':
		return character - 'a' + 10, nil
	case character >= 'A' && character <= 'F':
		return character - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("%q is not a hexadecimal digit", string(character))
	}
}

// Line joins fields into one record. Every field is encoded, so no field
// can contain the separator.
func Line(fields ...string) string {
	encoded := make([]string, 0, len(fields))
	for _, field := range fields {
		encoded = append(encoded, Encode(field))
	}
	return strings.Join(encoded, "\t")
}

// Fields splits one record back into its fields, decoded.
func Fields(line string) ([]string, error) {
	parts := strings.Split(line, "\t")
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		decoded, err := Decode(part)
		if err != nil {
			return nil, err
		}
		fields = append(fields, decoded)
	}
	return fields, nil
}

// Sort orders records by their decoded bytes.
func Sort(lines []string) {
	sort.Slice(lines, func(i, j int) bool {
		left, leftErr := Fields(lines[i])
		right, rightErr := Fields(lines[j])
		if leftErr != nil || rightErr != nil {
			return lines[i] < lines[j]
		}
		return strings.Join(left, "\x00") < strings.Join(right, "\x00")
	})
}

// SortNames orders plain names by their bytes. The same order as Sort,
// for the lists that carry one field per line.
func SortNames(names []string) {
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
}

// Document renders a whole file: one record per line, each ending in a
// newline, so that a file with no records is empty rather than one blank
// line.
func Document(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// Parse splits a document into records, refusing the whole file if any
// line will not decode.
func Parse(data []byte) ([][]string, error) {
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	var records [][]string
	for number, line := range strings.Split(text, "\n") {
		fields, err := Fields(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number+1, err)
		}
		records = append(records, fields)
	}
	return records, nil
}
