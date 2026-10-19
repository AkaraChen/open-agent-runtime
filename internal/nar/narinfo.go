package nar

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// NarInfo is a parsed binary cache .narinfo file.
type NarInfo struct {
	StorePath   string
	URL         string
	Compression string
	NarHash     string
	NarSize     int64
	References  []string // basenames
	Sigs        []string
}

// ParseNarInfo parses the key/value narinfo format.
func ParseNarInfo(r io.Reader) (*NarInfo, error) {
	ni := &NarInfo{Compression: "bzip2"}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ": ")
		if !ok {
			continue
		}
		switch k {
		case "StorePath":
			ni.StorePath = v
		case "URL":
			ni.URL = v
		case "Compression":
			ni.Compression = v
		case "NarHash":
			ni.NarHash = v
		case "NarSize":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("narinfo: bad NarSize %q", v)
			}
			ni.NarSize = n
		case "References":
			ni.References = strings.Fields(v)
		case "Sig":
			ni.Sigs = append(ni.Sigs, v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if ni.StorePath == "" || ni.URL == "" || ni.NarHash == "" {
		return nil, fmt.Errorf("narinfo: incomplete")
	}
	return ni, nil
}

// Fingerprint is the string signed by binary cache keys.
func (ni *NarInfo) Fingerprint() string {
	refs := make([]string, len(ni.References))
	for i, r := range ni.References {
		refs[i] = "/nix/store/" + r
	}
	return fmt.Sprintf("1;%s;%s;%d;%s", ni.StorePath, ni.NarHash, ni.NarSize, strings.Join(refs, ","))
}

// Verify checks that at least one signature matches a trusted key
// ("name:base64pubkey").
func (ni *NarInfo) Verify(trusted []string) error {
	keys := map[string]ed25519.PublicKey{}
	for _, t := range trusted {
		name, b64, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		pk, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(pk) != ed25519.PublicKeySize {
			continue
		}
		keys[name] = pk
	}
	fp := []byte(ni.Fingerprint())
	for _, s := range ni.Sigs {
		name, b64, ok := strings.Cut(s, ":")
		if !ok {
			continue
		}
		pk, ok := keys[name]
		if !ok {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		if ed25519.Verify(pk, fp, sig) {
			return nil
		}
	}
	return fmt.Errorf("narinfo %s: no valid signature from a trusted key", ni.StorePath)
}

const base32Alphabet = "0123456789abcdfghijklmnpqrsvwxyz"

// DecodeBase32 decodes nix's base32 encoding.
func DecodeBase32(s string, size int) ([]byte, error) {
	out := make([]byte, size)
	for n := 0; n < len(s); n++ {
		c := s[len(s)-n-1]
		d := strings.IndexByte(base32Alphabet, c)
		if d < 0 {
			return nil, fmt.Errorf("invalid base32 character %q", c)
		}
		b := n * 5
		i, j := b/8, uint(b%8)
		if i >= size {
			if d != 0 {
				return nil, fmt.Errorf("base32 overflow")
			}
			continue
		}
		out[i] |= byte(d << j)
		if v := d >> (8 - j); v != 0 {
			if i+1 >= size {
				return nil, fmt.Errorf("base32 overflow")
			}
			out[i+1] |= byte(v)
		}
	}
	return out, nil
}

// ParseSHA256 parses "sha256:<nix-base32 or hex>" into raw bytes.
func ParseSHA256(h string) ([]byte, error) {
	algo, v, ok := strings.Cut(h, ":")
	if !ok || algo != "sha256" {
		return nil, fmt.Errorf("unsupported hash %q", h)
	}
	switch len(v) {
	case 52:
		return DecodeBase32(v, 32)
	case 64:
		b := make([]byte, 32)
		_, err := fmt.Sscanf(v, "%x", &b)
		return b, err
	}
	return nil, fmt.Errorf("unsupported sha256 encoding %q", h)
}
