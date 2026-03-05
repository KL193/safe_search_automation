package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	MaxPages   = 10
	MaxWorkers = 2
)

type KeywordItem struct {
	Category string
	Keyword  string
}

func main() {
	// Load keywords from file
	data, err := os.ReadFile("keywords.txt")
	if err != nil {
		log.Fatal("Could not open keywords.txt:", err)
	}

	// Parse lines, skip empty lines
	var keywords []KeywordItem
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				category := strings.TrimSpace(parts[0])
				keyword := strings.TrimSpace(parts[1])
				keywords = append(keywords, KeywordItem{Category: category, Keyword: keyword})
			}
		}
	}

	if len(keywords) == 0 {
		log.Fatal("No keywords found in keywords.txt")
	}

	fmt.Printf("Loaded %d keywords:\n", len(keywords))
	for _, kw := range keywords {
		fmt.Printf("  [%s] %s\n", kw.Category, kw.Keyword)
	}

	jobs := make(chan KeywordItem, len(keywords))
	var wg sync.WaitGroup

	// Use a map to store results organized by category
	results := make(map[string][]string)
	var mu sync.Mutex

	for w := 1; w <= MaxWorkers; w++ {
		wg.Add(1)
		go worker(jobs, &wg, &results, &mu)
	}
	for _, kw := range keywords {
		jobs <- kw
	}
	close(jobs)
	wg.Wait()

	// Write results organized by category
	file, _ := os.OpenFile("reachable_sites.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	defer file.Close()

	// Sort categories alphabetically
	var categories []string
	for cat := range results {
		categories = append(categories, cat)
	}
	sort.Strings(categories)

	// Write organized output
	for i, category := range categories {
		if i > 0 {
			file.WriteString("\n")
		}
		file.WriteString(fmt.Sprintf("=== %s ===\n", category))
		for _, result := range results[category] {
			file.WriteString(result + "\n")
		}
	}

	fmt.Println("\n✓ Done! Results saved to reachable_sites.txt")
}

func worker(jobs <-chan KeywordItem, wg *sync.WaitGroup, results *map[string][]string, mu *sync.Mutex) {
	defer wg.Done()
	client := &http.Client{Timeout: 10 * time.Second}

	for kwItem := range jobs {
		searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(kwItem.Keyword))

		for page := 1; page <= MaxPages; page++ {
			fmt.Printf("\n[Worker] Processing [%s] '%s' - Page %d\n", kwItem.Category, kwItem.Keyword, page)

			req, err := http.NewRequest("GET", searchURL, nil)
			if err != nil {
				log.Printf("Failed to create request: %v", err)
				break
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; rv:109.0) Gecko/20100101 Firefox/115.0")
			req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
			req.Header.Set("Accept-Language", "en-US,en;q=0.5")

			resp, err := client.Do(req)
			if err != nil {
				log.Printf("Failed to fetch '[%s] %s': %v", kwItem.Category, kwItem.Keyword, err)
				break
			}

			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			fmt.Printf("  [DEBUG] HTML size: %d bytes\n", len(body))

			doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
			if err != nil {
				log.Printf("Failed to parse HTML: %v", err)
				break
			}

			found := 0
			doc.Find("a.result__a").Each(func(_ int, s *goquery.Selection) {
				href, exists := s.Attr("href")
				if !exists {
					return
				}
				// Decode DDG redirect URL to get the real URL
				parsed, err := url.Parse("https://duckduckgo.com" + href)
				if err != nil {
					return
				}
				realURL := parsed.Query().Get("uddg")
				if realURL == "" {
					return
				}
				found++
				verifyAndLog(realURL, kwItem, client, results, mu)
			})

			fmt.Printf("  [DEBUG] Found %d links on page %d\n", found, page)

			// Find next page URL for pagination
			nextURL := ""
			doc.Find("form[action='/html/'] input[name='s']").Each(func(_ int, s *goquery.Selection) {
				val, _ := s.Attr("value")
				nextURL = fmt.Sprintf(
					"https://html.duckduckgo.com/html/?q=%s&s=%s&dc=1&v=1&o=json&api=/d.js",
					url.QueryEscape(kwItem.Keyword), val,
				)
			})

			if nextURL == "" {
				fmt.Printf("  No more pages for '[%s] %s'\n", kwItem.Category, kwItem.Keyword)
				break
			}
			searchURL = nextURL
			time.Sleep(2 * time.Second)
		}
	}
}

func verifyAndLog(link string, kwItem KeywordItem, client *http.Client, results *map[string][]string, mu *sync.Mutex) {
	resp, err := client.Head(link)
	if err != nil {
		errStr := err.Error()
		reason := "UNKNOWN ERROR"
		if strings.Contains(errStr, "connection refused") {
			reason = "BLOCKED (connection refused)"
		} else if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline") {
			reason = "BLOCKED (timeout)"
		} else if strings.Contains(errStr, "no such host") {
			reason = "BLOCKED (DNS blocked)"
		} else if strings.Contains(errStr, "reset by peer") {
			reason = "BLOCKED (reset by peer)"
		} else if strings.Contains(errStr, "certificate") {
			reason = "BLOCKED (SSL error)"
		}

		short := errStr
		if len(errStr) > 60 {
			short = errStr[:60] + "..."
		}

		fmt.Printf("  [%-28s] %s\n  └─ reason: %s\n", reason, link, short)
		resultStr := fmt.Sprintf("Category: %s | Keyword: %s | Status: %s | %s", kwItem.Category, kwItem.Keyword, reason, link)
		
		mu.Lock()
		(*results)[kwItem.Category] = append((*results)[kwItem.Category], resultStr)
		mu.Unlock()
		return
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	label := ""
	switch {
	case status == 200:
		label = "REACHABLE ✓"
	case status == 301 || status == 302 || status == 303:
		label = "REDIRECT"
	case status == 400:
		label = "BAD REQUEST"
	case status == 401:
		label = "UNAUTHORIZED"
	case status == 403:
		label = "FORBIDDEN"
	case status == 404:
		label = "NOT FOUND"
	case status == 429:
		label = "RATE LIMITED"
	case status >= 500:
		label = "SERVER ERROR"
	default:
		label = "OTHER"
	}

	fmt.Printf("  [%-12s | %d] %s\n", label, status, link)
	resultStr := fmt.Sprintf("Category: %s | Keyword: %s | Status: %s (%d) | %s", kwItem.Category, kwItem.Keyword, label, status, link)
	
	mu.Lock()
	(*results)[kwItem.Category] = append((*results)[kwItem.Category], resultStr)
	mu.Unlock()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
