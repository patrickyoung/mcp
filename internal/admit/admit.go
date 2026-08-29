package admit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const serverInfoKey = "io.modelcontextprotocol/serverInfo"

// Endpoint is the credential-free identity of one explicit MCP endpoint.
type Endpoint struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Path    string   `json:"path,omitempty"`
}

// Canonical returns compact JSON with stable object-key ordering.
func Canonical(src []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("more than one JSON value")
		}
		return nil, err
	}
	return json.Marshal(value)
}

// Digest binds a capability descriptor to its kind, endpoint, and the
// identity asserted by the server during discovery. Unrelated catalogue
// entries and cache timestamps deliberately do not affect it.
func Digest(kind string, endpoint Endpoint, discovery, descriptor []byte) (string, error) {
	var disc map[string]any
	if err := decode(discovery, &disc); err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	var desc any
	if err := decode(descriptor, &desc); err != nil {
		return "", fmt.Errorf("descriptor: %w", err)
	}

	var identity any
	if meta, ok := disc["_meta"].(map[string]any); ok {
		identity = meta[serverInfoKey]
	}
	if identity == nil {
		identity = disc["serverInfo"]
	}

	envelope := struct {
		Kind       string   `json:"kind"`
		Endpoint   Endpoint `json:"endpoint"`
		Server     any      `json:"server"`
		Descriptor any      `json:"descriptor"`
	}{kind, endpoint, identity, desc}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func decode(src []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("more than one JSON value")
		}
		return err
	}
	return nil
}
