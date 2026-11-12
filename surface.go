package main

import "strings"

// surfaceMapping maps an incoming OpenAI-compatible path to the Open WebUI
// path that serves it. An empty target means the request path is forwarded
// verbatim (paths that Open WebUI already namespaces under /api/v1 or /ollama).
type surfaceMapping struct {
	prefix string
	target string
}

// surfaceMappings is a deliberately explicit allow-list. Everything listed here
// was observed on a real Open WebUI deployment or documented upstream; unknown
// paths still return 404 instead of being proxied blindly.
var surfaceMappings = []surfaceMapping{
	// OpenAI-compatible layer that Open WebUI exposes on its own /v1 router.
	// chat/completions and models are handled by dedicated proxy code paths
	// (model normalization, streaming, selection) and are only listed so the
	// generic forwarder can still serve them when reached.
	{"/v1/embeddings/", ""},
	// Open WebUI mounts the Anthropic Messages API at /api/v1/messages only; its
	// bare /v1/messages route answers 405. The OpenAI Responses API is not
	// implemented upstream, so it is intentionally absent (returns 404).
	{"/v1/messages", "/api/v1/messages"},
	{"/v1/messages/count_tokens", "/api/v1/messages/count_tokens"},
	{"/v1/files/", ""},
	{"/v1/tools/", ""},
	{"/v1/knowledge/", ""},
	{"/v1/mcp/", ""},
	{"/v1/audio/", ""},

	// Legacy /api aliases advertised in the Open WebUI API docs.
	{"/v1/embeddings", "/api/embeddings"},
	{"/v1/tools/list", "/api/v1/tools/list"},
	{"/v1/tools", "/api/v1/tools"},
	{"/v1/mcp", "/api/v1/mcp/servers"},
	{"/v1/mcp/servers", "/api/v1/mcp/servers"},

	// Ollama native pass-through (/ollama/<api>) plus its OpenAI shim.
	{"/ollama/", ""},

	// Anything already addressed at the upstream prefix is served verbatim.
	{"/api/", ""},
}

// mapSurfacePath returns the upstream path for a supported surface request.
// Exact entries always win; prefix entries only apply to deeper sub-paths.
func mapSurfacePath(path string) (string, bool) {
	for _, mapping := range surfaceMappings {
		if mapping.prefix == path {
			return mapping.target, true
		}
	}
	for _, mapping := range surfaceMappings {
		if strings.HasSuffix(mapping.prefix, "/") && strings.HasPrefix(path, mapping.prefix) {
			// Deeper routes under an allowed prefix are forwarded verbatim so new
			// upstream endpoints keep working without a proxy release.
			return mapping.target, true
		}
	}
	return "", false
}
