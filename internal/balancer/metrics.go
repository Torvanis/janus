package balancer

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// ServerMetrics is what a member's /metrics endpoint told us.
type ServerMetrics struct {
	Engine  string // "vllm" | "llamacpp"
	Running float64
	Waiting float64
	// KVUsage is the KV-cache fill fraction 0..1.
	KVUsage float64
	// KVCapacityTokens is the number of tokens the server's KV cache holds
	// (vLLM: num_gpu_blocks x block_size from cache_config_info; llama.cpp:
	// n_ctx across all slots). 0 when not exposed.
	KVCapacityTokens float64
	// PrefixHits / PrefixQueries are cumulative prompt-cache counters (vLLM)
	// used to report the pool's cache hit rate.
	PrefixHits    float64
	PrefixQueries float64
	// Found reports that at least one recognised load metric was present.
	Found bool
}

// ParseMetrics reads a Prometheus text exposition from a vLLM or llama.cpp
// server and extracts the load signals. Unknown lines are ignored, and
// several metric names are accepted per signal because both engines have
// renamed them across releases. Values from several model_name label sets
// (vLLM can serve more than one) are summed.
func ParseMetrics(r io.Reader) ServerMetrics {
	var m ServerMetrics
	var blocks, blockSize, sizeTokens float64
	var kvSeen bool
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, value, ok := splitSample(line)
		if !ok {
			continue
		}
		switch name {
		case "vllm:num_requests_running":
			m.Engine, m.Running, m.Found = "vllm", m.Running+value, true
		case "vllm:num_requests_waiting":
			m.Engine, m.Waiting, m.Found = "vllm", m.Waiting+value, true
		case "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc":
			if !kvSeen || value > m.KVUsage {
				m.KVUsage = value
			}
			kvSeen, m.Engine, m.Found = true, "vllm", true
		case "vllm:prefix_cache_hits_total", "vllm:prefix_cache_hits", "vllm:gpu_prefix_cache_hits_total":
			m.PrefixHits += value
		case "vllm:prefix_cache_queries_total", "vllm:prefix_cache_queries", "vllm:gpu_prefix_cache_queries_total":
			m.PrefixQueries += value
		case "vllm:cache_config_info":
			blocks = labelFloat(labels, "num_gpu_blocks")
			blockSize = labelFloat(labels, "block_size")
			sizeTokens = labelFloat(labels, "kv_cache_size_tokens")
		case "llamacpp:requests_processing":
			m.Engine, m.Running, m.Found = "llamacpp", m.Running+value, true
		case "llamacpp:requests_deferred":
			m.Engine, m.Waiting, m.Found = "llamacpp", m.Waiting+value, true
		case "llamacpp:kv_cache_usage_ratio":
			m.Engine, m.KVUsage, m.Found = "llamacpp", value, true
			// llamacpp:n_tokens_max is the largest request seen, NOT capacity;
			// llama.cpp capacity comes from /slots.
		}
	}
	switch {
	case sizeTokens > 0:
		// Newer vLLM states the capacity directly. Prefer it: for hybrid
		// (attention + mamba) models blocks x block_size overstates it
		// (Qwen3.8-27B: 208 x 784 = 163,072 vs 141,994 real).
		m.KVCapacityTokens = sizeTokens
	case blocks > 0 && blockSize > 0:
		m.KVCapacityTokens = blocks * blockSize
	}
	return m
}

// splitSample splits `name{labels} value [timestamp]`.
func splitSample(line string) (name, labels string, value float64, ok bool) {
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", "", 0, false
		}
		name, labels, rest = line[:i], line[i+1:j], strings.TrimSpace(line[j+1:])
	} else {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "", "", 0, false
		}
		name, rest = fields[0], strings.Join(fields[1:], " ")
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", "", 0, false
	}
	return name, labels, v, true
}

// labelFloat reads one numeric label value from a label string.
func labelFloat(labels, key string) float64 {
	for _, part := range splitLabels(labels) {
		k, v, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(k) != key {
			continue
		}
		f, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(v), `"`), 64)
		if err == nil {
			return f
		}
	}
	return 0
}

// splitLabels splits on commas outside quotes.
func splitLabels(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && quoted && i+1 < len(s):
			b.WriteByte(c)
			i++
			b.WriteByte(s[i])
			continue
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
