package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"context-lens/backend/exchange"
	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

// ArtifactMatch is a byte-offset match returned by the optional artifact
// search operation.  End is exclusive, matching ArtifactRead's range shape.
type ArtifactMatch struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// ArtifactSearchResult is returned when an artifact request includes a
// search/q query.  Search never changes the stored artifact and only returns
// offsets, not body bytes.
type ArtifactSearchResult struct {
	ArtifactID string          `json:"artifact_id"`
	Query      string          `json:"query"`
	Matches    []ArtifactMatch `json:"matches"`
	Truncated  bool            `json:"truncated"`
	TotalSize  int64           `json:"total_size"`
	Complete   bool            `json:"complete"`
}

type apiError struct {
	Error            string `json:"error"`
	Code             string `json:"code,omitempty"`
	ExpectedRevision uint64 `json:"expected_revision,omitempty"`
	ReceivedRevision uint64 `json:"received_revision,omitempty"`
	ExpectedArtifact string `json:"expected_artifact_id,omitempty"`
	ReceivedArtifact string `json:"received_artifact_id,omitempty"`
}

// ServeHTTP routes all workspace API endpoints.  Endpoint paths are:
//
//	GET  /api/exchanges
//	GET  /api/exchanges/{id}
//	POST /api/exchanges/{id}/command (and /commands)
//	GET  /api/artifacts/{id}
//	GET/PUT /api/policy
//	GET  /api/events (SSE)
//
// The configured Prefix can be changed for an embedding process.  A few
// singular/plural command aliases are accepted for compatibility with small
// browser clients, but all responses keep the frozen runtime JSON names.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		http.NotFound(w, r)
		return
	}
	if r == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				writeAPIError(w, http.StatusForbidden, "cross_origin_denied", "workspace mutation requires same origin")
				return
			}
		}
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
		if mediaType != "application/json" {
			writeAPIError(w, http.StatusUnsupportedMediaType, "json_required", "workspace mutation requires application/json")
			return
		}
	}
	if r.Method == http.MethodOptions {
		s.handleOptions(w)
		return
	}
	prefix := s.config.Prefix
	rest, ok := routeRest(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Accept /api/workspace/... as a compatibility mount while keeping the
	// canonical paths directly below Prefix.
	if strings.HasPrefix(rest, "/workspace/") {
		rest = strings.TrimPrefix(rest, "/workspace")
	}
	if rest == "" || rest == "/" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"service": "context-lens-workspace",
			"events":  prefix + "/events",
		})
		return
	}

	switch {
	case rest == "/exchanges":
		s.handleExchangeList(w, r)
	case strings.HasPrefix(rest, "/exchanges/"):
		s.handleExchangePath(w, r, strings.TrimPrefix(rest, "/exchanges/"))
	case strings.HasPrefix(rest, "/commands/"):
		s.handleCommandPathAlias(w, r, strings.TrimPrefix(rest, "/commands/"))
	case rest == "/artifacts" || rest == "/artifact":
		s.handleArtifact(w, r, "")
	case strings.HasPrefix(rest, "/artifacts/"):
		s.handleArtifact(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "/artifacts/"), "/body"))
	case strings.HasPrefix(rest, "/artifact/"):
		s.handleArtifact(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "/artifact/"), "/body"))
	case rest == "/policy" || rest == "/settings/policy":
		s.handlePolicy(w, r)
	case rest == "/events" || rest == "/events/stream" || rest == "/workspace/events":
		s.handleEvents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func routeRest(requestPath, prefix string) (string, bool) {
	if requestPath == "" {
		requestPath = "/"
	}
	if prefix == "/" {
		return requestPath, true
	}
	if requestPath == prefix {
		return "", true
	}
	if !strings.HasPrefix(requestPath, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(requestPath, prefix), true
}

func (s *Server) handleOptions(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID, Range")
	w.Header().Set("Access-Control-Max-Age", "300")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExchangeList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.backend == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "backend_unavailable", "exchange backend is not configured")
		return
	}
	items, err := s.listExchanges(r.Context())
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	limit, offset, err := parseListWindow(r.URL.Query(), s.config.MaxExchanges)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_window", err.Error())
		return
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	items = items[offset:end]
	redacted := make([]exchange.Snapshot, len(items))
	for i, item := range items {
		redacted[i] = redactSnapshot(item)
	}
	if redacted == nil {
		redacted = []exchange.Snapshot{}
	}
	if r.Method == http.MethodHead {
		writeJSONHead(w, http.StatusOK, redacted)
		return
	}
	s.writeJSON(w, http.StatusOK, redacted)
}

func parseListWindow(values url.Values, max int) (int, int, error) {
	if max <= 0 {
		max = 1000
	}
	limit := max
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return 0, 0, errors.New("limit must be a non-negative integer")
		}
		if n > max {
			n = max
		}
		limit = n
	}
	offset := 0
	if raw := values.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return 0, 0, errors.New("offset must be a non-negative integer")
		}
		offset = n
	}
	return limit, offset, nil
}

func (s *Server) handleExchangePath(w http.ResponseWriter, r *http.Request, encoded string) {
	parts := strings.Split(strings.TrimSuffix(encoded, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	exchangeID, err := safePathID(parts[0])
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_exchange_id", "invalid exchange id")
		return
	}
	if len(parts) == 1 {
		s.handleExchangeGet(w, r, exchangeID)
		return
	}
	if len(parts) == 2 && (parts[1] == "command" || parts[1] == "commands") {
		s.handleCommand(w, r, exchangeID)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleCommandPathAlias(w http.ResponseWriter, r *http.Request, encoded string) {
	parts := strings.Split(strings.TrimSuffix(encoded, "/"), "/")
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	id, err := safePathID(parts[0])
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_exchange_id", "invalid exchange id")
		return
	}
	s.handleCommand(w, r, id)
}
func (s *Server) handleExchangeGet(w http.ResponseWriter, r *http.Request, exchangeID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.backend == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "backend_unavailable", "exchange backend is not configured")
		return
	}
	snapshot, err := s.getExchange(r.Context(), exchangeID)
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	snapshot = redactSnapshot(snapshot)
	if r.Method == http.MethodHead {
		writeJSONHead(w, http.StatusOK, snapshot)
		return
	}
	s.writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request, exchangeID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if s.backend == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "backend_unavailable", "exchange backend is not configured")
		return
	}
	var raw map[string]json.RawMessage
	reader := io.ReadCloser(http.NoBody)
	if r.Body != nil {
		reader = r.Body
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, reader, s.config.MaxRequestBytes))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "command_too_large", "command body exceeds configured limit")
		return
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "command body is not valid JSON")
		return
	}
	if raw == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_command", "command object is required")
		return
	}
	if _, ok := raw["base_revision"]; !ok {
		writeAPIError(w, http.StatusBadRequest, "missing_base_revision", "base_revision is required")
		return
	}
	var command exchange.Command
	if err := json.Unmarshal(body, &command); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_command", "command shape is invalid")
		return
	}
	if command.ExchangeID == "" {
		command.ExchangeID = exchangeID
	}
	if command.ExchangeID != exchangeID {
		writeAPIError(w, http.StatusBadRequest, "exchange_id_mismatch", "exchange_id does not match URL")
		return
	}
	if command.Kind == "" {
		writeAPIError(w, http.StatusBadRequest, "missing_command_kind", "kind is required")
		return
	}

	result, err := s.command(r.Context(), command)
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	result.Exchange = redactSnapshot(result.Exchange)
	if result.Event == nil {
		event := eventForCommand(command, result)
		result.Event = &event
	}
	event := s.Publish(redactEvent(*result.Event))
	result.Event = eventPtr(redactEvent(event))
	result.Mutation = redactMutation(result.Mutation)
	s.writeJSON(w, http.StatusOK, result)
}

func eventForCommand(command exchange.Command, result exchange.CommandResult) exchange.Event {
	snapshot := result.Exchange
	kind := exchange.EventUpdated
	switch snapshot.State {
	case exchange.StateCompleted:
		kind = exchange.EventCompleted
	case exchange.StateDropped:
		kind = exchange.EventDropped
	case exchange.StateCancelled:
		kind = exchange.EventCancelled
	case exchange.StateFailed:
		kind = exchange.EventFailed
	case exchange.StateRequestHeld:
		kind = exchange.EventRequestHeld
	case exchange.StateResponseHeld:
		kind = exchange.EventResponseHeld
	case exchange.StateUpstreamRunning:
		kind = exchange.EventUpstreamStarted
	}
	delta := exchange.SnapshotDelta{
		ExchangeID: snapshot.ExchangeID,
		Protocol:   snapshot.Protocol,
		State:      snapshot.State,
		Warnings:   append([]string(nil), snapshot.Warnings...),
		UpdatedAt:  snapshot.UpdatedAt,
		Error:      snapshot.Error,
	}
	var refs []wire.ArtifactRef
	if result.Mutation != nil && result.Mutation.DerivedArtifact != nil {
		refs = []wire.ArtifactRef{*result.Mutation.DerivedArtifact}
	}
	return exchange.Event{
		EventID:       fmt.Sprintf("%s:%d", command.ExchangeID, result.Revision),
		ExchangeID:    command.ExchangeID,
		Revision:      result.Revision,
		Kind:          kind,
		SnapshotDelta: delta,
		ArtifactRefs:  refs,
		CreatedAt:     snapshot.UpdatedAt,
	}
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if s.policy == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "policy_unavailable", "policy store is not configured")
			return
		}
		p := s.policy.Get().Normalize()
		if r.Method == http.MethodHead {
			writeJSONHead(w, http.StatusOK, p)
			return
		}
		s.writeJSON(w, http.StatusOK, p)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPut+", "+http.MethodPatch+", "+http.MethodPost)
		return
	}
	if s.policy == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "policy_unavailable", "policy store is not configured")
		return
	}
	reader := io.ReadCloser(http.NoBody)
	if r.Body != nil {
		reader = r.Body
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, reader, s.config.MaxRequestBytes))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "policy_too_large", "policy body exceeds configured limit")
		return
	}
	var decoded struct {
		RequestGate  policy.GateMode `json:"request_gate"`
		ResponseGate policy.GateMode `json:"response_gate"`
		Policy       *policy.Policy  `json:"policy,omitempty"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "policy body is not valid JSON")
		return
	}
	next := policy.Policy{RequestGate: decoded.RequestGate, ResponseGate: decoded.ResponseGate}
	if r.Method == http.MethodPatch {
		current := s.policy.Get().Normalize()
		if next.RequestGate == "" {
			next.RequestGate = current.RequestGate
		}
		if next.ResponseGate == "" {
			next.ResponseGate = current.ResponseGate
		}
	}
	if decoded.Policy != nil {
		next = *decoded.Policy
	}
	if err := next.Validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_policy", "request_gate and response_gate must be pass or hold")
		return
	}
	if err := s.policy.Set(next.Normalize()); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_policy", "request_gate and response_gate must be pass or hold")
		return
	}
	s.writeJSON(w, http.StatusOK, s.policy.Get().Normalize())
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request, idFromPath string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	artifactID := idFromPath
	if artifactID == "" {
		artifactID = r.URL.Query().Get("artifact_id")
	}
	artifactID, err := safePathID(artifactID)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_artifact_id", "invalid artifact id")
		return
	}
	if s.artifacts == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "artifact_store_unavailable", "artifact store is not configured")
		return
	}
	query := r.URL.Query().Get("search")
	if query == "" {
		query = r.URL.Query().Get("q")
	}
	if query != "" {
		s.handleArtifactSearch(w, r, artifactID, query)
		return
	}
	s.handleArtifactBody(w, r, artifactID)
}

func (s *Server) handleArtifactSearch(w http.ResponseWriter, r *http.Request, artifactID, query string) {
	if len(query) > int(s.config.MaxSearchBytes) {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "search_too_large", "search query exceeds configured limit")
		return
	}
	searchLimit := s.config.MaxSearchMatches
	if searchLimit < int(^uint(0)>>1) {
		searchLimit++
	}
	matches, ref, err := s.searchArtifact(r.Context(), artifactID, []byte(query), searchLimit)
	if err != nil {
		s.writeArtifactError(w, err)
		return
	}
	result := ArtifactSearchResult{
		ArtifactID: artifactID,
		Query:      query,
		Matches:    matches,
		Truncated:  false,
		TotalSize:  ref.Size,
		Complete:   ref.Complete,
	}
	if len(matches) > s.config.MaxSearchMatches {
		result.Truncated = true
		result.Matches = matches[:s.config.MaxSearchMatches]
	}
	if result.Matches == nil {
		result.Matches = []ArtifactMatch{}
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleArtifactBody(w http.ResponseWriter, r *http.Request, artifactID string) {
	ref, body, start, end, partial, err := s.readArtifactRequest(r.Context(), r, artifactID)
	if err != nil {
		s.writeArtifactError(w, err)
		return
	}
	setArtifactHeaders(w, ref, start, end, partial, r.URL.Query().Get("download"))
	if partial {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func (s *Server) readArtifactRequest(ctx context.Context, r *http.Request, artifactID string) (wire.ArtifactRef, []byte, int64, int64, bool, error) {
	ref, err := s.artifactRef(ctx, artifactID)
	if err != nil {
		return wire.ArtifactRef{}, nil, 0, 0, false, err
	}
	if ref.Size < 0 {
		return wire.ArtifactRef{}, nil, 0, 0, false, ErrArtifactInvalid
	}
	start, end, partial, err := parseArtifactRange(r, ref.Size)
	if err != nil {
		return wire.ArtifactRef{}, nil, 0, 0, false, err
	}
	if end-start > s.config.MaxArtifactBytes {
		return wire.ArtifactRef{}, nil, 0, 0, false, ErrArtifactTooLarge
	}
	if rangeStore, ok := s.artifacts.(RangeArtifactStore); ok {
		body, err := rangeStore.ReadRange(ctx, artifactID, start, end)
		if err != nil {
			return wire.ArtifactRef{}, nil, 0, 0, false, err
		}
		if int64(len(body)) != end-start {
			return wire.ArtifactRef{}, nil, 0, 0, false, ErrArtifactInvalid
		}
		return ref, body, start, end, partial, nil
	}
	artifact, err := s.artifacts.Get(ctx, artifactID)
	if err != nil {
		return wire.ArtifactRef{}, nil, 0, 0, false, err
	}
	actual := artifact.Ref()
	if actual.Size != ref.Size || actual.SHA256 != ref.SHA256 {
		return wire.ArtifactRef{}, nil, 0, 0, false, ErrArtifactInvalid
	}
	if actual.Size > s.config.MaxArtifactBytes && !partial {
		return wire.ArtifactRef{}, nil, 0, 0, false, ErrArtifactTooLarge
	}
	full := artifact.Bytes()
	if end > int64(len(full)) {
		return wire.ArtifactRef{}, nil, 0, 0, false, ErrArtifactInvalid
	}
	return ref, append([]byte(nil), full[start:end]...), start, end, partial, nil
}

func parseArtifactRange(r *http.Request, size int64) (int64, int64, bool, error) {
	if size < 0 {
		return 0, 0, false, ErrArtifactInvalid
	}
	if raw := r.URL.Query().Get("range"); raw != "" {
		if !strings.HasPrefix(raw, "bytes=") {
			return 0, 0, false, ErrArtifactRange
		}
		start, end, err := parseHTTPRange(strings.TrimPrefix(raw, "bytes="), size)
		if err != nil {
			return 0, 0, false, err
		}
		return start, end, true, nil
	}
	start, end := int64(0), size
	hasStart, hasEnd := false, false
	if raw := r.URL.Query().Get("start"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, ErrArtifactRange
		}
		start, hasStart = n, true
	}
	if raw := r.URL.Query().Get("end"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, ErrArtifactRange
		}
		end, hasEnd = n, true
	}
	if raw := r.Header.Get("Range"); raw != "" {
		if !strings.HasPrefix(raw, "bytes=") {
			return 0, 0, false, ErrArtifactRange
		}
		var err error
		start, end, err = parseHTTPRange(strings.TrimPrefix(raw, "bytes="), size)
		if err != nil {
			return 0, 0, false, err
		}
		return start, end, true, nil
	}
	if start > size || end < start {
		return 0, 0, false, ErrArtifactRange
	}
	// An end beyond the blob is a valid bounded preview request; clamp it to
	// the available bytes rather than making the browser know the size first.
	if end > size {
		end = size
	}
	// Explicit `end` is exclusive, as used by the browser contract.  An
	// omitted end means the end of the blob.
	return start, end, hasStart || hasEnd, nil
}

func parseHTTPRange(raw string, size int64) (int64, int64, error) {
	if strings.Contains(raw, ",") {
		return 0, 0, ErrArtifactRange // one range keeps response semantics clear
	}
	parts := strings.SplitN(strings.TrimSpace(raw), "-", 2)
	if len(parts) != 2 {
		return 0, 0, ErrArtifactRange
	}
	if size == 0 {
		return 0, 0, ErrArtifactRange
	}
	if parts[0] == "" {
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, ErrArtifactRange
		}
		if n > size {
			n = size
		}
		return size - n, size, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, ErrArtifactRange
	}
	end := size
	if parts[1] != "" {
		last, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || last < start {
			return 0, 0, ErrArtifactRange
		}
		if last < size-1 {
			end = last + 1 // HTTP Range end is inclusive
		}
	}
	return start, end, nil
}

func setArtifactHeaders(w http.ResponseWriter, ref wire.ArtifactRef, start, end int64, partial bool, download string) {
	contentType := ref.ContentType
	if strings.ContainsAny(contentType, "\r\n") || strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start, 10))
	w.Header().Set("X-Artifact-ID", ref.ArtifactID)
	w.Header().Set("X-Artifact-SHA256", ref.SHA256)
	w.Header().Set("X-Artifact-Complete", strconv.FormatBool(ref.Complete))
	w.Header().Set("X-Artifact-Total-Size", strconv.FormatInt(ref.Size, 10))
	// Content-Encoding is intentionally not copied to the HTTP response.  The
	// endpoint returns exact stored application bytes; setting it would invite
	// clients/transports to transparently decode the authority.  Expose the
	// metadata under a non-transforming header instead.
	if ref.ContentEncoding != "" && !strings.ContainsAny(ref.ContentEncoding, "\r\n") {
		w.Header().Set("X-Artifact-Content-Encoding", ref.ContentEncoding)
	}
	if len(ref.SHA256) == 64 && !strings.ContainsAny(ref.SHA256, "\r\n") {
		w.Header().Set("ETag", `"`+ref.SHA256+`"`)
	}
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, ref.Size))
	}
	// Artifact bodies are data, never active same-origin documents. Fetch clients
	// can still read them while top-level navigation is forced to download.
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(ref.ArtifactID)+`"`)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	_ = download
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func safeFilename(id string) string {
	var b strings.Builder
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "artifact"
	}
	return b.String()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.events == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "events_unavailable", "event broker is not configured")
		return
	}
	lastID := r.Header.Get("Last-Event-ID")
	if lastID == "" {
		lastID = r.URL.Query().Get("last_event_id")
	}
	stream, cancel := s.events.Subscribe(lastID)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "stream_unsupported", "response writer does not support streaming")
		return
	}
	_, _ = io.WriteString(w, ": connected\nretry: 3000\n\n")
	flusher.Flush()
	ticker := time.NewTicker(s.config.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case event, ok := <-stream:
			if !ok {
				return
			}
			if err := writeSSE(w, redactEvent(event)); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, event exchange.Event) error {
	payload, err := json.Marshal(redactEvent(event))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", sanitizeSSEField(event.EventID), sanitizeSSEField(string(event.Kind)), payload); err != nil {
		return err
	}
	return nil
}

func sanitizeSSEField(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return value
}

func (s *Server) artifactRef(ctx context.Context, artifactID string) (wire.ArtifactRef, error) {
	var (
		ref wire.ArtifactRef
		err error
	)
	if rangeStore, ok := s.artifacts.(RangeArtifactStore); ok {
		ref, err = rangeStore.ArtifactRef(ctx, artifactID)
	} else {
		artifact, getErr := s.artifacts.Get(ctx, artifactID)
		if getErr != nil {
			return wire.ArtifactRef{}, getErr
		}
		ref = artifact.Ref()
	}
	if err != nil {
		return wire.ArtifactRef{}, err
	}
	if err := validateArtifactRef(ref, artifactID); err != nil {
		return wire.ArtifactRef{}, err
	}
	return ref, nil
}

func validateArtifactRef(ref wire.ArtifactRef, expectedID string) error {
	if ref.ArtifactID != expectedID {
		return ErrArtifactInvalid
	}
	if err := ref.Validate(); err != nil {
		return ErrArtifactInvalid
	}
	return nil
}

func (s *Server) searchArtifact(ctx context.Context, artifactID string, query []byte, limit int) ([]ArtifactMatch, wire.ArtifactRef, error) {
	if rangeStore, ok := s.artifacts.(RangeArtifactStore); ok {
		ref, err := rangeStore.ArtifactRef(ctx, artifactID)
		if err != nil {
			return nil, wire.ArtifactRef{}, err
		}
		matches, err := rangeStore.Search(ctx, artifactID, query, limit)
		return matches, ref, err
	}
	artifact, err := s.artifacts.Get(ctx, artifactID)
	if err != nil {
		return nil, wire.ArtifactRef{}, err
	}
	ref := artifact.Ref()
	if ref.Size > s.config.MaxSearchBytes {
		return nil, wire.ArtifactRef{}, ErrArtifactTooLarge
	}
	body := artifact.Bytes()
	matches := make([]ArtifactMatch, 0)
	for at := 0; at+len(query) <= len(body) && len(matches) < limit; {
		idx := bytes.Index(body[at:], query)
		if idx < 0 {
			break
		}
		start := at + idx
		matches = append(matches, ArtifactMatch{Start: int64(start), End: int64(start + len(query))})
		at = start + 1
	}
	return matches, ref, nil
}

func (s *Server) listExchanges(ctx context.Context) ([]exchange.Snapshot, error) {
	if backend, ok := s.backend.(ContextExchangeBackend); ok {
		return backend.ListExchangesContext(ctx)
	}
	return s.backend.ListExchanges()
}

func (s *Server) getExchange(ctx context.Context, id string) (exchange.Snapshot, error) {
	if backend, ok := s.backend.(ContextExchangeBackend); ok {
		return backend.GetExchangeContext(ctx, id)
	}
	return s.backend.GetExchange(id)
}

func (s *Server) command(ctx context.Context, command exchange.Command) (exchange.CommandResult, error) {
	if backend, ok := s.backend.(ContextExchangeBackend); ok {
		return backend.CommandContext(ctx, command)
	}
	return s.backend.Command(command)
}

func (s *Server) writeBackendError(w http.ResponseWriter, err error) {
	status, code, message := classifyBackendError(err)
	api := apiError{Error: message, Code: code}
	var revision *exchange.RevisionConflictError
	if errors.As(err, &revision) {
		api.ExpectedRevision = revision.Expected
		api.ReceivedRevision = revision.Received
	}
	var artifact *exchange.ArtifactConflictError
	if errors.As(err, &artifact) {
		api.ExpectedArtifact = artifact.ExpectedID
		api.ReceivedArtifact = artifact.ReceivedID
	}
	s.writeJSON(w, status, api)
}

func classifyBackendError(err error) (int, string, string) {
	switch {
	case errors.Is(err, exchange.ErrNotFound):
		return http.StatusNotFound, "not_found", "exchange not found"
	case errors.Is(err, exchange.ErrRevisionConflict):
		return http.StatusConflict, "revision_conflict", "stale revision"
	case errors.Is(err, exchange.ErrArtifactConflict):
		return http.StatusConflict, "artifact_conflict", "stale artifact"
	case errors.Is(err, exchange.ErrInvalidCommand), errors.Is(err, exchange.ErrMutationInvalid):
		return http.StatusBadRequest, "invalid_command", "invalid command"
	case errors.Is(err, exchange.ErrInvalidState), errors.Is(err, exchange.ErrAlreadyTerminal), errors.Is(err, exchange.ErrNoResponse), errors.Is(err, exchange.ErrUpstreamNotStarted):
		return http.StatusConflict, "invalid_state", "command is not valid in the current exchange state"
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, "cancelled", "request cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout", "backend operation timed out"
	default:
		return http.StatusBadGateway, "backend_error", "exchange backend operation failed"
	}
}

func (s *Server) writeArtifactError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusBadRequest, "artifact_error", "artifact request failed"
	switch {
	case errors.Is(err, ErrArtifactNotFound), errors.Is(err, exchange.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "artifact not found"
	case errors.Is(err, ErrArtifactTooLarge):
		status, code, message = http.StatusRequestEntityTooLarge, "artifact_too_large", "artifact exceeds configured body limit"
	case errors.Is(err, ErrArtifactRange):
		status, code, message = http.StatusRequestedRangeNotSatisfiable, "invalid_range", "artifact range is invalid"
	case errors.Is(err, ErrArtifactInvalid):
		status, code, message = http.StatusUnprocessableEntity, "invalid_artifact", "stored artifact is invalid"
	case errors.Is(err, context.Canceled):
		status, code, message = http.StatusRequestTimeout, "cancelled", "request cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, "timeout", "artifact operation timed out"
	}
	writeAPIError(w, status, code, message)
}

func redactSnapshot(in exchange.Snapshot) exchange.Snapshot {
	out := in
	out.Request = in.Request
	out.Request.Envelope = in.Request.Envelope.Redacted()
	out.Request.ArtifactRefs = append([]wire.ArtifactRef(nil), in.Request.ArtifactRefs...)
	out.Response = in.Response
	out.Response.Envelope = in.Response.Envelope.Redacted()
	out.Response.ArtifactRefs = append([]wire.ArtifactRef(nil), in.Response.ArtifactRefs...)
	out.Warnings = append([]string(nil), in.Warnings...)
	return out
}

func redactEvent(in exchange.Event) exchange.Event {
	out := in
	out.SnapshotDelta = redactDelta(in.SnapshotDelta)
	out.ArtifactRefs = append([]wire.ArtifactRef(nil), in.ArtifactRefs...)
	if in.Stream != nil {
		stream := *in.Stream
		out.Stream = &stream
	}
	return out
}

func redactDelta(in exchange.SnapshotDelta) exchange.SnapshotDelta {
	out := in
	if in.Request != nil {
		req := *in.Request
		req.Envelope = in.Request.Envelope.Redacted()
		req.ArtifactRefs = append([]wire.ArtifactRef(nil), in.Request.ArtifactRefs...)
		out.Request = &req
	}
	if in.Response != nil {
		resp := *in.Response
		resp.Envelope = in.Response.Envelope.Redacted()
		resp.ArtifactRefs = append([]wire.ArtifactRef(nil), in.Response.ArtifactRefs...)
		out.Response = &resp
	}
	out.Warnings = append([]string(nil), in.Warnings...)
	return out
}

func redactMutation(in *exchange.MutationResult) *exchange.MutationResult {
	if in == nil {
		return nil
	}
	out := *in
	if in.DerivedArtifact != nil {
		ref := *in.DerivedArtifact
		out.DerivedArtifact = &ref
	}
	if in.Validation != nil {
		validation := *in.Validation
		validation.Errors = append([]string(nil), in.Validation.Errors...)
		validation.Warnings = append([]string(nil), in.Validation.Warnings...)
		out.Validation = &validation
	}
	return &out
}

func eventPtr(event exchange.Event) *exchange.Event { return &event }

func safePathID(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty id")
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\\x00\r\n") {
		return "", errors.New("invalid id")
	}
	if len(decoded) > 256 {
		return "", errors.New("id too long")
	}
	for _, r := range decoded {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("invalid id")
		}
	}
	return decoded, nil
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "encode_error", "response encoding failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSONHead(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "encode_error", "response encoding failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(apiError{Error: message, Code: code})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
}

// Keep path imported in this file as a compile-time guard for future route
// additions that use path.Clean only after validating encoded ids.
var _ = path.Clean
