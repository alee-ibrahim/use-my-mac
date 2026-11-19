package hostprep

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// DecodePlist decodes an XML property list (as `diskutil -plist` prints it)
// into map[string]any, []any, string, int64, float64 and bool values.
func DecodePlist(b []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("plist: no value")
			}
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			return plistValue(d, se)
		}
	}
}

func plistValue(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		key := ""
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					var k string
					if err := d.DecodeElement(&k, &t); err != nil {
						return nil, err
					}
					key = k
					continue
				}
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
			case xml.EndElement:
				return m, nil
			}
		}
	case "array":
		var a []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			case xml.EndElement:
				return a, nil
			}
		}
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	var s string
	if err := d.DecodeElement(&s, &se); err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "integer":
		return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	case "real":
		return strconv.ParseFloat(strings.TrimSpace(s), 64)
	case "string", "date", "data":
		return s, nil
	}
	return nil, fmt.Errorf("plist: unsupported element <%s>", se.Name.Local)
}

// str returns m[key] as a string ("" if absent or not a string).
func str(v any, key string) string {
	m, _ := v.(map[string]any)
	s, _ := m[key].(string)
	return s
}

// isAddr reports whether s is an IP address or CIDR.
func isAddr(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(s)
	return err == nil
}
