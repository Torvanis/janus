package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/endpoints"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Batch jobs and fine-tuning jobs do their work at the provider after the
// call that starts them has returned. Two things follow.
//
// A batch names its models inside an uploaded file, where the grant check
// that runs on every ordinary request never looks. So before a batch is
// forwarded the gateway reads the file and checks every line.
//
// Neither job reports usage on the call that starts it. The gateway follows
// each job up until it ends and records what it used then (SettleStoredJobs).

// jobDetail is what settlement needs to bill a job to whoever started it.
type jobDetail struct {
	UserID         string   `json:"user_id,omitempty"`
	TokenID        string   `json:"token_id,omitempty"`
	ServiceTokenID string   `json:"service_token_id,omitempty"`
	TeamIDs        string   `json:"team_ids,omitempty"`
	GroupIDs       []string `json:"group_ids,omitempty"`
	RequestID      string   `json:"request_id,omitempty"`
	ClientApp      string   `json:"client_app,omitempty"`
	// Endpoint is the inference endpoint a batch's lines call; Models are the
	// models its lines named.
	Endpoint string   `json:"endpoint,omitempty"`
	Models   []string `json:"models,omitempty"`
}

func jobDetailFor(event *store.UsageEvent) jobDetail {
	return jobDetail{
		UserID: event.UserID, TokenID: event.TokenID, ServiceTokenID: event.ServiceTokenID,
		TeamIDs: event.TeamIDs, GroupIDs: event.GroupIDs, RequestID: event.RequestID, ClientApp: event.ClientApp,
	}
}

// batchPriceFactor is the share of the ordinary token price a batch request
// costs. Providers that offer batches price them at half (OpenAI, Anthropic,
// Mistral, Together, Groq). It stands in until a model's price sheet carries
// its own batch price.
const batchPriceFactor = 0.5

// Batch input limits, as the providers set them.
const (
	maxBatchLines     = 50000
	maxBatchFileBytes = 200 << 20
)

// upstreamCall makes a gateway-originated GET to a model's upstream with the
// upstream's own credential (reading a batch's input file, following a job up).
func (s *Server) upstreamCall(ctx context.Context, model *store.Model, path string) (*http.Response, error) {
	upstream, err := s.cachedUpstreamByID(ctx, model.UpstreamID)
	if err != nil {
		return nil, err
	}
	if !upstream.Enabled {
		return nil, errors.New("the upstream is disabled")
	}
	providerAdapter, err := adapter.Get(upstream.AdapterType)
	if err != nil {
		return nil, err
	}
	apiKey, err := s.Cipher.Decrypt(upstream.EncryptedKey())
	if err != nil {
		return nil, err
	}
	// A status poll is Janus's own call: no caller headers, so the policy's are added here.
	client := s.ownUpstreamClient(ctx, upstream.ID, upstream.BaseURL)
	prepared, err := providerAdapter.Prepare(adapter.Upstream{
		ID: upstream.ID, Name: upstream.Name, BaseURL: upstream.BaseURL, APIKey: apiKey, AdapterType: upstream.AdapterType,
	}, &adapter.Request{Method: http.MethodGet, Path: path, Header: http.Header{}, Model: model.Name, Context: ctx, Client: client})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prepared.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = prepared.Header
	return client.Do(req)
}

// batchLine is one request of a batch input file.
type batchLine struct {
	CustomID string          `json:"custom_id"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// batchPreflight reads a batch's input file and holds every line to what an
// ordinary request is held to: a known endpoint, a model the caller is
// granted, and references only to the caller's own objects. It returns the
// models the batch uses.
func (s *Server) batchPreflight(ctx context.Context, body []byte, serving *store.Model, granted map[string]string,
	eng *secgw.Engine, event *store.UsageEvent, groupIDs []string, ownerType, ownerID string, found *[]secgw.Violation) (jobDetail, *APIError) {
	detail := jobDetailFor(event)
	var req struct {
		InputFileID string `json:"input_file_id"`
		Endpoint    string `json:"endpoint"`
	}
	if json.Unmarshal(body, &req) != nil || req.InputFileID == "" {
		return detail, ErrInvalidRequest("A batch needs an input_file_id.").WithParam("input_file_id")
	}
	if !endpoints.BatchEndpoint(req.Endpoint) {
		return detail, ErrInvalidRequest("Batches can target /v1/chat/completions, /v1/completions, /v1/responses, /v1/embeddings or /v1/moderations.").WithParam("endpoint")
	}
	detail.Endpoint = req.Endpoint
	if eng.FamilyCovers(event.UserID) {
		return detail, ErrForbidden("Batch jobs are not available on this account: Child Safety checks every message, and the requests inside a batch cannot be checked.")
	}
	resp, err := s.upstreamCall(ctx, serving, "/v1/files/"+req.InputFileID+"/content")
	if err != nil {
		s.Logger.WarnContext(ctx, "read batch input file", "error", err.Error(), "request_id", event.RequestID)
		return detail, ErrUpstreamUnavailable(serving.UpstreamName)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return detail, ErrInvalidRequest("The batch input file could not be read from the provider (HTTP " + strconv.Itoa(resp.StatusCode) + "). Upload it with purpose \"batch\".").WithParam("input_file_id")
	}
	reader := bufio.NewReaderSize(io.LimitReader(resp.Body, maxBatchFileBytes+1), 64<<10)
	models := map[string]*store.Model{}
	var total int64
	for n := 1; ; n++ {
		line, rerr := reader.ReadBytes('\n')
		total += int64(len(line))
		if total > maxBatchFileBytes {
			return detail, ErrInvalidRequest("The batch input file is larger than 200 MB.").WithParam("input_file_id")
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if n > maxBatchLines {
				return detail, ErrInvalidRequest("The batch input file has more than 50,000 requests.").WithParam("input_file_id")
			}
			if apiErr := s.checkBatchLine(ctx, n, trimmed, req.Endpoint, serving, granted, eng, event, groupIDs, ownerType, ownerID, models, found); apiErr != nil {
				return detail, apiErr
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return detail, ErrUpstreamUnavailable(serving.UpstreamName)
			}
			break
		}
	}
	if len(models) == 0 {
		return detail, ErrInvalidRequest("The batch input file has no requests.").WithParam("input_file_id")
	}
	for name := range models {
		detail.Models = append(detail.Models, name)
	}
	return detail, nil
}

func (s *Server) checkBatchLine(ctx context.Context, n int, raw []byte, endpoint string, serving *store.Model, granted map[string]string,
	eng *secgw.Engine, event *store.UsageEvent, groupIDs []string, ownerType, ownerID string, models map[string]*store.Model, found *[]secgw.Violation) *APIError {
	where := "Line " + strconv.Itoa(n) + " of the batch input file"
	var line batchLine
	if json.Unmarshal(raw, &line) != nil || len(line.Body) == 0 {
		return ErrInvalidRequest(where + " is not a batch request (an object with custom_id, method, url and body).")
	}
	if line.URL != endpoint {
		return ErrInvalidRequest(where + " calls " + truncate(line.URL, 80) + ", but the batch is for " + endpoint + ".")
	}
	named, apiErr := resolveJSONModel(line.Body, false, "")
	if apiErr != nil {
		return ErrInvalidRequest(where + ": " + apiErr.Message)
	}
	if !named.FromBody {
		return ErrInvalidRequest(where + " names no model.").WithParam("model")
	}
	if _, seen := models[named.Name]; !seen {
		// The provider resolves the name itself, so it must be the model's
		// own name there: a display name or managed model would not exist.
		model, err := s.Store.ModelByUpstreamAndName(ctx, serving.UpstreamID, named.Name)
		if err != nil || model.Status != store.ModelEnabled || model.ClassifierRole != "" {
			return ErrModelNotGranted(named.Name)
		}
		if _, ok := granted[model.ID]; !ok {
			return ErrModelNotGranted(named.Name)
		}
		if kind, ok := eng.Resolve(secgwSubject(event, groupIDs, store.ResolvedModel{Model: model})).BlockingModelCheck(); ok {
			// A check that asks a model about each message and stops what
			// it flags cannot run on requests that are made later.
			return ErrForbidden("Batch jobs are not available for " + named.Name + ": the Security Gateway's " + secgwKindLabel(kind) +
				" check stops flagged messages as they are sent, and the requests inside a batch are sent later.")
		}
		models[named.Name] = model
	}
	if apiErr := s.checkReferences(ctx, ownerType, ownerID, line.Body); apiErr != nil {
		return apiErr
	}
	// The secrets, personal-data and term checks read the line as they
	// would read the request it holds.
	vs, stop := eng.InspectDeferred(eng.Resolve(secgwSubject(event, groupIDs, store.ResolvedModel{Model: models[named.Name]})), line.Body)
	if room := maxBatchViolations - len(*found); room > 0 {
		*found = append(*found, vs[:min(len(vs), room)]...)
	}
	if stop != "" {
		apiErr := ErrSecurityBlocked(stop, false)
		apiErr.Message = where + " was stopped by the Security Gateway (" + secgwKindLabel(stop) + "). Change it and upload the file again."
		return apiErr
	}
	return nil
}

// maxBatchViolations bounds what one batch records: a file can hold 50,000
// requests.
const maxBatchViolations = 200

// secgwKindLabel names a check for a person.
func secgwKindLabel(kind store.SecgwCheckKind) string {
	switch kind {
	case store.SecgwCheckSecrets:
		return "secrets"
	case store.SecgwCheckPII:
		return "personal data"
	case store.SecgwCheckTerms:
		return "blocked terms"
	case store.SecgwCheckPromptInjection:
		return "prompt injection"
	case store.SecgwCheckContentSafety:
		return "content safety"
	}
	return string(kind)
}

// --- settlement ----------------------------------------------------------------

// jobSettleInterval is how often open jobs are followed up; jobClaimHold is
// how long one replica keeps a job to itself while it does so.
const (
	jobSettleInterval = 2 * time.Minute
	jobClaimHold      = 5 * time.Minute
	// jobAbandonAfter closes a job the provider never finished: batches
	// expire within a day or two, and a week covers a paused fine-tune.
	jobAbandonAfter = 14 * 24 * time.Hour
)

// StartJobSettlement follows open batch and fine-tuning jobs up until the
// server stops.
func (s *Server) StartJobSettlement(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(jobSettleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.SettleStoredJobs(ctx)
			}
		}
	}()
}

// SettleStoredJobs asks the provider about each open job and records the
// usage of those that have ended. It returns how many it closed.
func (s *Server) SettleStoredJobs(ctx context.Context) int {
	jobs, err := s.Store.ClaimObjectJobs(ctx, 25, jobClaimHold)
	if err != nil {
		if ctx.Err() == nil {
			s.Logger.WarnContext(ctx, "claim open provider jobs", "error", err.Error())
		}
		return 0
	}
	closed := 0
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		if s.settleJob(ctx, job) {
			closed++
		}
	}
	return closed
}

// providerJob is the part of a batch or fine-tuning job object settlement reads.
type providerJob struct {
	Status        string   `json:"status"`
	OutputFileID  string   `json:"output_file_id"`
	ErrorFileID   string   `json:"error_file_id"`
	TrainedTokens int64    `json:"trained_tokens"`
	ResultFiles   []string `json:"result_files"`
}

func jobEnded(kind, status string) bool {
	switch status {
	case "completed", "failed", "expired", "cancelled":
		return kind == endpoints.FamilyBatch || status != "completed"
	case "succeeded":
		return kind == endpoints.FamilyFineTune
	}
	return false
}

func (s *Server) settleJob(ctx context.Context, job *store.ObjectJob) bool {
	log := s.Logger.With("kind", job.Kind, "object_id", job.ObjectID, "upstream_id", job.UpstreamID)
	abandon := func(why string) bool {
		log.WarnContext(ctx, "closing a provider job without usage", "reason", why)
		closed, _ := s.Store.SettleObjectJob(ctx, job.UpstreamID, job.ObjectID)
		return closed
	}
	model, err := s.Store.ModelByID(ctx, job.ModelID)
	if err != nil {
		return abandon("its model no longer exists")
	}
	path := "/v1/batches/" + job.ObjectID
	if job.Kind == endpoints.FamilyFineTune {
		path = "/v1/fine_tuning/jobs/" + job.ObjectID
	}
	resp, err := s.upstreamCall(ctx, model, path)
	if err != nil {
		log.WarnContext(ctx, "follow up provider job", "error", err.Error())
		return false
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return abandon("the provider no longer has it")
	}
	var state providerJob
	if resp.StatusCode != http.StatusOK || json.Unmarshal(payload, &state) != nil {
		log.WarnContext(ctx, "follow up provider job", "status", resp.StatusCode)
		return false
	}
	if !jobEnded(job.Kind, state.Status) {
		if time.Since(job.CreatedAt) > jobAbandonAfter {
			return abandon("it never finished")
		}
		return false
	}
	var detail jobDetail
	_ = json.Unmarshal([]byte(job.Detail), &detail)

	// What the job produced belongs to whoever started it.
	derived := append([]string{state.OutputFileID, state.ErrorFileID}, state.ResultFiles...)
	for _, id := range derived {
		if id == "" || len(id) > maxObjectIDBytes {
			continue
		}
		if err := s.Store.RecordUpstreamObject(ctx, store.UpstreamObject{
			UpstreamID: job.UpstreamID, ObjectID: id, Kind: endpoints.FamilyFile, OwnerType: job.OwnerType, OwnerID: job.OwnerID,
			ModelID: job.ModelID, AccessID: job.AccessID, RequestedModel: job.RequestedModel, ParentID: job.ObjectID,
		}); err != nil {
			log.WarnContext(ctx, "record a job's output file", "error", err.Error())
		}
	}

	var events []*store.UsageEvent
	switch job.Kind {
	case endpoints.FamilyBatch:
		if state.OutputFileID != "" {
			events, err = s.batchUsageEvents(ctx, job, detail, model, state.OutputFileID)
			if err != nil {
				log.WarnContext(ctx, "read batch output", "error", err.Error())
				return false // try again on the next round
			}
		}
	case endpoints.FamilyFineTune:
		if state.TrainedTokens > 0 {
			event := s.jobEvent(ctx, job, detail, model, "/v1/fine_tuning/jobs", 0)
			event.Modality = adapter.ModalityFineTune
			// Training has its own price, which the rate card does not
			// carry: the tokens are recorded and the cost left unknown.
			event.TokensIn = state.TrainedTokens
			event.AccountingMode = usage.AccountingUnmetered
			if !s.Config.LocalOnly {
				event.CostStatus = "unknown"
			}
			events = append(events, event)
		}
	}
	// Closing comes first and only one caller wins it, so a job is never
	// billed twice even if two replicas reach this point together.
	if closed, err := s.Store.SettleObjectJob(ctx, job.UpstreamID, job.ObjectID); err != nil || !closed {
		return false
	}
	subject := quota.UserSubject(detail.UserID, splitNonEmpty(detail.TeamIDs))
	if detail.ServiceTokenID != "" {
		subject = quota.ServiceTokenSubject(detail.ServiceTokenID)
	}
	for _, event := range events {
		eventModel, merr := s.Store.ModelByID(ctx, event.ModelID)
		if merr != nil {
			eventModel = model
		}
		s.recordEvent(ctx, event, subject, eventModel, nil)
	}
	log.InfoContext(ctx, "provider job settled", "status", state.Status, "usage_events", len(events))
	return true
}

func splitNonEmpty(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// jobEvent starts the usage event for a job's work on one model.
func (s *Server) jobEvent(ctx context.Context, job *store.ObjectJob, detail jobDetail, model *store.Model, path string, n int) *store.UsageEvent {
	event := &store.UsageEvent{
		EndpointPath: path, HTTPMethod: http.MethodPost, HTTPStatus: http.StatusOK,
		Modality:  usage.ModalityForPath(detail.Endpoint),
		RequestID: truncate(job.Kind+":"+job.ObjectID+":"+strconv.Itoa(n), maxRequestIDLen),
		UserID:    detail.UserID, TokenID: detail.TokenID, ServiceTokenID: detail.ServiceTokenID,
		TeamIDs: detail.TeamIDs, GroupIDs: append([]string{}, detail.GroupIDs...), ClientApp: detail.ClientApp,
	}
	s.stampServedModel(ctx, event, model)
	if job.AccessID != "" && job.AccessID != job.ModelID {
		event.ManagedModelID, event.RequestedModelName = job.AccessID, job.RequestedModel
	}
	return event
}

// batchUsageEvents reads a finished batch's output and sums what each model
// used: one usage event per model, priced at the batch rate.
func (s *Server) batchUsageEvents(ctx context.Context, job *store.ObjectJob, detail jobDetail, serving *store.Model, outputFileID string) ([]*store.UsageEvent, error) {
	resp, err := s.upstreamCall(ctx, serving, "/v1/files/"+outputFileID+"/content")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("output file answered HTTP %d", resp.StatusCode)
	}
	providerAdapter, err := adapter.Get(serving.AdapterType)
	if err != nil {
		return nil, err
	}
	type sum struct {
		usage    adapter.Usage
		requests int
		// quoted counts the results that carried a quoted cost; their sum
		// is the batch's cost only when every result quoted one.
		quoted    int
		quoteNano int64
	}
	sums := map[string]*sum{}
	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, rerr := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var out struct {
				Response struct {
					StatusCode int             `json:"status_code"`
					Body       json.RawMessage `json:"body"`
				} `json:"response"`
			}
			if json.Unmarshal(trimmed, &out) == nil && len(out.Response.Body) > 0 {
				var served struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(out.Response.Body, &served)
				name := requestedBatchModel(served.Model, detail.Models)
				if used, ok := providerAdapter.ExtractUsage(out.Response.Body); ok {
					entry := sums[name]
					if entry == nil {
						entry = &sum{}
						sums[name] = entry
					}
					entry.requests++
					entry.usage.Reported = true
					entry.usage.TokensIn += used.TokensIn
					entry.usage.TokensOut += used.TokensOut
					entry.usage.TokensCached += used.TokensCached
					entry.usage.TokensCacheWrite5m += used.TokensCacheWrite5m
					entry.usage.TokensCacheWrite1h += used.TokensCacheWrite1h
					if used.CostReported {
						entry.quoted++
						entry.quoteNano += used.CostNano
					}
				}
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return nil, rerr
			}
			break
		}
	}
	var events []*store.UsageEvent
	n := 0
	for name, entry := range sums {
		model, merr := s.Store.ModelByUpstreamAndName(ctx, job.UpstreamID, name)
		if merr != nil {
			model = serving
		}
		event := s.jobEvent(ctx, job, detail, model, "/v1/batches", n)
		n++
		rates := usage.Rates{
			InNano: model.RateInNano, OutNano: model.RateOutNano, CachedNano: model.RateCachedNano,
			CacheWrite5mNano: model.RateCacheWrite5mNano, CacheWrite1hNano: model.RateCacheWrite1hNano,
			Lines: model.PriceLines(),
		}
		card := s.cachedRateCard(ctx, model.ID)
		if card.OK {
			rates = card.rates()
		}
		rates = rates.Scaled(batchPriceFactor)
		stampReportingCost(event, rates, card.OK, s.Config.LocalOnly)
		if entry.quoted > 0 && entry.quoted == entry.requests {
			// Every result quoted what it cost (xAI batch results carry
			// cost_in_usd_ticks, the batch discount applied): their sum is
			// the cost, not the rate card at the batch factor. A batch where
			// only some results quote is priced by the card as a whole, so
			// no result is counted twice or missed.
			entry.usage.CostNano, entry.usage.CostReported = entry.quoteNano, true
		}
		s.applyUsageMetered(event, entry.usage, rates, -1)
		events = append(events, event)
	}
	return events, nil
}

// requestedBatchModel maps the model a batch response names (often a dated
// snapshot such as gpt-4o-mini-2024-07-18) back to the model the line asked for.
func requestedBatchModel(served string, requested []string) string {
	best := ""
	for _, name := range requested {
		if name == served {
			return name
		}
		if strings.HasPrefix(served, name) && len(name) > len(best) {
			best = name
		}
	}
	if best != "" {
		return best
	}
	if len(requested) > 0 {
		return requested[0]
	}
	return served
}
