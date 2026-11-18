package main

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// usageWindow is how many minute buckets the console keeps. An hour of history is
// enough to answer "is this website failing right now" without keeping a log.
const usageWindow = 60

// defaultUsageSite labels traffic from accounts that have no WebsiteID.
const defaultUsageSite = "(mặc định)"

// usageBucket counts requests routed to one website in one minute.
type usageBucket struct {
	Success int `json:"success"`
	Failed  int `json:"failed"`
}

// usageStats tracks per-website request outcomes for the console. It is bounded
// (websites × usageWindow buckets) and in-memory only: the proxy never persists
// request history.
type usageStats struct {
	mu      sync.Mutex
	bySite  map[string]map[int64]*usageBucket
	perSite map[string]*usageTotal
	minute  func() int64
}

type usageTotal struct {
	Success    int64     `json:"success"`
	Failed     int64     `json:"failed"`
	LastSeen   time.Time `json:"last_seen"`
	LastStatus int       `json:"last_status"`
	LastError  string    `json:"last_error,omitempty"`
	LastModel  string    `json:"last_model,omitempty"`
}

func newUsageStats() *usageStats {
	return &usageStats{
		bySite:  make(map[string]map[int64]*usageBucket),
		perSite: make(map[string]*usageTotal),
		minute:  func() int64 { return time.Now().Unix() / 60 },
	}
}

// record files one request outcome for a website. An account that belongs to no
// website (a single-account install without WebsiteConfig) is still counted, under
// a stable placeholder, so the console never shows an empty table while traffic
// is flowing.
func (u *usageStats) record(website string, status int, model, errText string) {
	if u == nil {
		return
	}
	if website == "" {
		website = defaultUsageSite
	}
	ok := status >= 200 && status < 300
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.minute()
	buckets, known := u.bySite[website]
	if !known {
		buckets = make(map[int64]*usageBucket)
		u.bySite[website] = buckets
	}
	bucket := buckets[now]
	if bucket == nil {
		bucket = &usageBucket{}
		buckets[now] = bucket
		// Drop buckets that fell out of the window so a long-running proxy does
		// not grow a bucket per website per minute forever.
		cutoff := now - usageWindow
		for when := range buckets {
			if when < cutoff {
				delete(buckets, when)
			}
		}
	}
	if ok {
		bucket.Success++
	} else {
		bucket.Failed++
	}
	total := u.perSite[website]
	if total == nil {
		total = &usageTotal{}
		u.perSite[website] = total
	}
	if ok {
		total.Success++
	} else {
		total.Failed++
		total.LastError = errText
	}
	total.LastSeen = time.Now()
	total.LastStatus = status
	if model != "" {
		total.LastModel = model
	}
}

// snapshot renders the console payload: totals plus a full minute series per
// website (zero-filled so the UI can draw a line without preprocessing).
func (u *usageStats) snapshot() []map[string]any {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.minute()
	sites := make([]string, 0, len(u.perSite))
	for site := range u.perSite {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	out := make([]map[string]any, 0, len(sites))
	for _, site := range sites {
		total := u.perSite[site]
		buckets := make([]usageBucket, usageWindow)
		if stored, ok := u.bySite[site]; ok {
			for i := 0; i < usageWindow; i++ {
				if bucket, ok := stored[now-int64(usageWindow-1-i)]; ok && bucket != nil {
					buckets[i] = *bucket
				}
			}
		}
		entry := map[string]any{
			"website":     site,
			"success":     total.Success,
			"failed":      total.Failed,
			"last_seen":   total.LastSeen.UTC().Format(time.RFC3339),
			"last_status": total.LastStatus,
			"minutes":     buckets,
		}
		if total.LastError != "" {
			entry["last_error"] = total.LastError
		}
		if total.LastModel != "" {
			entry["last_model"] = total.LastModel
		}
		out = append(out, entry)
	}
	return out
}

// usageHandler serves GET /api/admin/usage.
func (a *Application) usageHandler(w http.ResponseWriter) {
	p, _, _ := a.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"object":         "list",
		"window_minutes": usageWindow,
		"data":           p.usage.snapshot(),
	})
}
