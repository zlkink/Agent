package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// webSearch runs the toolbus web_search tool. The provider and key live in
// host settings (Settings → Chat); DuckDuckGo's HTML endpoint needs no key,
// so search works out of the box, while Tavily/Brave/SearXNG give better
// results and page text.
func (h *Host) webSearch(ctx context.Context, query string, count int) ([]any, error) {
	h.mu.Lock()
	provider := strings.ToLower(str(h.settings["webSearchProvider"]))
	key := str(h.settings["webSearchApiKey"])
	base := strings.TrimRight(str(h.settings["webSearchBaseUrl"]), "/")
	maxResults := int(number(h.settings["webSearchMaxResults"]))
	h.mu.Unlock()
	if provider == "" {
		provider = "duckduckgo"
	}
	if count <= 0 || count > 10 {
		count = maxResults
	}
	if count <= 0 || count > 10 {
		count = 5
	}
	switch provider {
	case "tavily", "brave", "searxng", "duckduckgo":
	default:
		return nil, fmt.Errorf("未知的网页搜索服务：%s", provider)
	}
	if provider == "tavily" || provider == "brave" {
		if key == "" {
			return nil, fmt.Errorf("网页搜索 %s 需要 API Key，请在 设置 → 对话 中填写", provider)
		}
	}
	if provider == "searxng" && base == "" {
		return nil, fmt.Errorf("SearXNG 需要实例地址，请在 设置 → 对话 中填写")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	switch provider {
	case "tavily":
		if base == "" {
			base = "https://api.tavily.com"
		}
		body, _ := json.Marshal(map[string]any{"api_key": key, "query": query, "max_results": count, "search_depth": "basic"})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/search", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return doSearchJSON(client, req, func(b []byte) ([]any, error) {
			var out struct {
				Results []searchHit `json:"results"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, err
			}
			return searchHits(out.Results), nil
		})
	case "brave":
		if base == "" {
			base = "https://api.search.brave.com"
		}
		u := base + "/res/v1/web/search?" + url.Values{"q": {query}, "count": {fmt.Sprint(count)}}.Encode()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("X-Subscription-Token", key)
		return doSearchJSON(client, req, func(b []byte) ([]any, error) {
			var out struct {
				Web struct {
					Results []searchHit `json:"results"`
				} `json:"web"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, err
			}
			return searchHits(out.Web.Results), nil
		})
	case "searxng":
		u := base + "/search?" + url.Values{"q": {query}, "format": {"json"}}.Encode()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		return doSearchJSON(client, req, func(b []byte) ([]any, error) {
			var out struct {
				Results []searchHit `json:"results"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, err
			}
			return searchHits(out.Results), nil
		})
	default:
		return duckDuckGoSearch(ctx, client, query, count)
	}
}

func doSearchJSON(client *http.Client, req *http.Request, parse func([]byte) ([]any, error)) ([]any, error) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "RamiCore/1.0")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("搜索接口返回 HTTP %d", res.StatusCode)
	}
	return parse(b)
}

// searchHits maps provider structs by reflection-free field names, since the
// three providers use different JSON shapes. result holds title/url and one of
// content/description.
type searchHit struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Content     string `json:"content"`
	Description string `json:"description"`
}

func searchHits(rows []searchHit) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		text := r.Content
		if text == "" {
			text = r.Description
		}
		out = append(out, map[string]any{"title": r.Title, "url": r.URL, "snippet": clipSearchText(text, 600)})
	}
	return out
}

func duckDuckGoSearch(ctx context.Context, client *http.Client, query string, count int) ([]any, error) {
	u := "https://html.duckduckgo.com/html/?" + url.Values{"q": {query}}.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("DuckDuckGo 返回 HTTP %d", res.StatusCode)
	}
	page := string(b)
	linkRe := regexp.MustCompile(`(?is)<a[^>]+class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	snippetRe := regexp.MustCompile(`(?is)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	links := linkRe.FindAllStringSubmatch(page, -1)
	snippets := snippetRe.FindAllStringSubmatch(page, -1)
	out := make([]any, 0, min(count, len(links)))
	tagRe := regexp.MustCompile(`(?s)<[^>]+>`)
	for i, m := range links {
		if len(out) >= count {
			break
		}
		href := html.UnescapeString(m[1])
		if strings.HasPrefix(href, "//duckduckgo.com/l/?uddg=") {
			if parsed, err := url.Parse("https:" + href); err == nil {
				if target := parsed.Query().Get("uddg"); target != "" {
					href = target
				}
			}
		}
		title := strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(m[2], "")))
		snippet := ""
		if i < len(snippets) {
			snippet = strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(snippets[i][1], "")))
		}
		out = append(out, map[string]any{"title": title, "url": href, "snippet": clipSearchText(snippet, 600)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("DuckDuckGo 没有返回结果（可能被网络限制），可在设置中改用 Tavily/Brave/SearXNG")
	}
	return out, nil
}

func clipSearchText(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return string(r)
}
