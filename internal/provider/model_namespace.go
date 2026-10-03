package provider

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// resolvedModel is a publisher-qualified upstream model resolved from a public,
// namespaced model ID. Publisher drives upstream endpoint and format selection.
type resolvedModel struct {
	Publisher  string
	UpstreamID string
}

// exposedModelID builds the public, namespaced model ID from its parts. The
// publisher segment keeps Claude and Gemini catalogs collision-free and lets the
// executor route without a catalog lookup, e.g. "vertex/anthropic/claude-sonnet-4".
func exposedModelID(prefix, publisher, upstreamID string) string {
	return prefix + "/" + publisher + "/" + upstreamID
}

// splitPublisherModel splits a publisher-qualified identifier such as
// "anthropic/claude-sonnet-4@20250514" into its publisher and upstream model ID.
func splitPublisherModel(name string) (publisher, upstreamID string, ok bool) {
	publisher, upstreamID, found := strings.Cut(strings.TrimSpace(name), "/")
	publisher = strings.ToLower(strings.TrimSpace(publisher))
	upstreamID = strings.TrimSpace(upstreamID)
	if !found || publisher == "" || upstreamID == "" {
		return "", "", false
	}
	return publisher, upstreamID, true
}

func (c Config) exposedModelID(publisher, upstreamID string) string {
	return exposedModelID(c.ModelPrefix, publisher, upstreamID)
}

func (c Config) modelAliases(publisher, upstreamID string) []string {
	if len(c.Models) == 0 {
		return []string{c.exposedModelID(publisher, upstreamID)}
	}
	qualified := publisher + "/" + upstreamID
	var aliases []string
	for _, model := range c.Models {
		if strings.EqualFold(model.Name, qualified) {
			aliases = append(aliases, model.Alias)
		}
	}
	return aliases
}

// Handles reports whether the given public model ID belongs to this plugin, so
// the model router can claim it and route to this executor without a built-in auth.
func (c Config) Handles(exposedID string) bool {
	_, err := c.resolveModel(exposedID)
	return err == nil
}

// resolveModel maps a public model ID back to its publisher and upstream ID.
func (c Config) resolveModel(exposedID string) (resolvedModel, error) {
	exposedID = strings.TrimSpace(exposedID)
	if len(c.Models) > 0 {
		for _, model := range c.Models {
			if model.Alias == exposedID {
				publisher, upstreamID, ok := splitPublisherModel(model.Name)
				if !ok {
					return resolvedModel{}, statusError("model_not_found", "configured model is malformed", 500)
				}
				return resolvedModel{Publisher: publisher, UpstreamID: upstreamID}, nil
			}
		}
		return resolvedModel{}, statusError("model_not_found", "model is not in the configured allowlist", 404)
	}
	rest, ok := strings.CutPrefix(exposedID, c.ModelPrefix+"/")
	if !ok {
		return resolvedModel{}, statusError("model_not_found", "Vertex models must use the configured "+c.ModelPrefix+"/ namespace", 404)
	}
	publisher, upstreamID, ok := splitPublisherModel(rest)
	if !ok {
		return resolvedModel{}, statusError("model_not_found", "model ID must be "+c.ModelPrefix+"/<publisher>/<model>", 404)
	}
	if _, supported := supportedPublishers[publisher]; !supported {
		return resolvedModel{}, statusError("model_not_found", "unsupported publisher "+publisher, 404)
	}
	return resolvedModel{Publisher: publisher, UpstreamID: upstreamID}, nil
}

// Only protocol model metadata is rewritten. Message text, function arguments,
// tool results, and other user-controlled JSON remain untouched.
func rewriteResponseModel(payload []byte, model string) ([]byte, error) {
	for _, path := range []string{"model", "message.model", "response.model"} {
		value := gjson.GetBytes(payload, path)
		if value.Type != gjson.String || value.String() == model {
			continue
		}
		var err error
		payload, err = sjson.SetBytes(payload, path, model)
		if err != nil {
			return nil, err
		}
	}
	return payload, nil
}

// Rewrite complete SSE frames without changing event/id/retry/comment lines.
// A multiline JSON data field is compacted to one valid data line when changed.
func rewriteStreamModel(frame []byte, model string) ([]byte, error) {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var data [][]byte
	first := -1
	for i, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if first < 0 {
			first = i
		}
		value := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		value = bytes.TrimPrefix(value, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		data = append(data, value)
	}
	payload := bytes.Join(data, []byte("\n"))
	if first < 0 || !json.Valid(payload) {
		return frame, nil
	} // Includes [DONE] and keepalives.
	updated, err := rewriteResponseModel(payload, model)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(updated, payload) {
		return frame, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, updated); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for i, line := range lines {
		if i == first {
			out.WriteString("data: ")
			out.Write(compact.Bytes())
			if bytes.HasSuffix(line, []byte("\r\n")) {
				out.WriteString("\r\n")
			} else if bytes.HasSuffix(line, []byte("\n")) {
				out.WriteByte('\n')
			}
		} else if !bytes.HasPrefix(line, []byte("data:")) {
			out.Write(line)
		}
	}
	return out.Bytes(), nil
}
