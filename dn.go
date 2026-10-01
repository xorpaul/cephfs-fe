package main

// Distinguished names in the RFC 4514 string form, e.g.
// "UID=1000,CN=alice,O=Example Org": RDNs from the last
// to the first as encoded in the certificate (the order openssl prints with
// -nameopt RFC2253). pkix.Name.String() is not used because it prints
// attributes it has no name for, such as UID, as hex-encoded OIDs.

import (
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"strings"
)

// dnNames maps attribute OIDs to their RFC 4514 / RFC 4519 short names.
var dnNames = map[string]string{
	"2.5.4.3":                    "CN",
	"2.5.4.4":                    "SN",
	"2.5.4.5":                    "SERIALNUMBER",
	"2.5.4.6":                    "C",
	"2.5.4.7":                    "L",
	"2.5.4.8":                    "ST",
	"2.5.4.9":                    "STREET",
	"2.5.4.10":                   "O",
	"2.5.4.11":                   "OU",
	"2.5.4.12":                   "TITLE",
	"2.5.4.42":                   "GIVENNAME",
	"0.9.2342.19200300.100.1.1":  "UID",
	"0.9.2342.19200300.100.1.25": "DC",
	"1.2.840.113549.1.9.1":       "EMAILADDRESS",
}

// dnAliases maps other spellings of attribute names, e.g. "E" and
// OID forms, to the names subjectDN uses.
var dnAliases = map[string]string{
	"E":      "EMAILADDRESS",
	"EMAIL":  "EMAILADDRESS",
	"USERID": "UID",
}

func init() {
	for oid, name := range dnNames {
		dnAliases[oid] = name
		dnAliases["OID."+oid] = name
	}
}

// subjectDN formats a certificate's raw subject.
func subjectDN(rawSubject []byte) (string, error) {
	var seq pkix.RDNSequence
	rest, err := asn1.Unmarshal(rawSubject, &seq)
	if err != nil {
		return "", err
	}
	if len(rest) > 0 {
		return "", fmt.Errorf("trailing data after subject")
	}
	rdns := make([]string, 0, len(seq))
	for i := len(seq) - 1; i >= 0; i-- {
		var avas []string
		for _, atv := range seq[i] {
			typ, ok := dnNames[atv.Type.String()]
			if !ok {
				typ = atv.Type.String()
			}
			val, ok := atv.Value.(string)
			if !ok {
				val = fmt.Sprint(atv.Value)
			}
			avas = append(avas, typ+"="+escapeDNValue(val))
		}
		rdns = append(rdns, strings.Join(avas, "+"))
	}
	return strings.Join(rdns, ","), nil
}

// canonicalDN parses a DN written by a person and returns it in the form
// subjectDN produces: attribute names upper-cased, no spaces around the
// separators, values re-escaped. Values stay case-sensitive.
func canonicalDN(s string) (string, error) {
	var rdns []string
	var avas []string
	var cur strings.Builder
	flush := func() error {
		t, v, ok := strings.Cut(cur.String(), "=")
		t = strings.TrimSpace(t)
		if !ok || t == "" {
			return fmt.Errorf("%q: want TYPE=value", cur.String())
		}
		// Only unescaped surrounding spaces are insignificant; escaped ones
		// were turned into \x00-marked placeholders below.
		v = strings.TrimSpace(v)
		v = strings.ReplaceAll(v, "\x00", "")
		if v == "" {
			return fmt.Errorf("%s has an empty value", t)
		}
		t = strings.ToUpper(t)
		if a, ok := dnAliases[t]; ok {
			t = a
		}
		avas = append(avas, t+"="+escapeDNValue(v))
		cur.Reset()
		return nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			// \XX hex pair or \c; mark the escaped byte so TrimSpace keeps
			// an escaped space.
			ch := s[i+1]
			if i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
				fmt.Sscanf(s[i+1:i+3], "%02x", &ch)
				i++
			}
			i++
			cur.WriteByte('\x00')
			cur.WriteByte(ch)
			cur.WriteByte('\x00')
		case c == '+':
			if err := flush(); err != nil {
				return "", err
			}
		case c == ',' || c == ';':
			if err := flush(); err != nil {
				return "", err
			}
			rdns = append(rdns, strings.Join(avas, "+"))
			avas = nil
		default:
			cur.WriteByte(c)
		}
	}
	if err := flush(); err != nil {
		return "", err
	}
	rdns = append(rdns, strings.Join(avas, "+"))
	return strings.Join(rdns, ","), nil
}

// escapeDNValue escapes a value as RFC 4514 section 2.4 requires.
func escapeDNValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case strings.IndexByte(`,+"\<>;`, c) >= 0,
			c == '#' && i == 0,
			c == ' ' && (i == 0 || i == len(v)-1):
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == 0:
			b.WriteString(`\00`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
