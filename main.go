package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	MaxPages   = 10
	MaxWorkers = 2
)

func main() {
	// Load keywords from file
	data, err := os.ReadFile("keywords.txt")
	if err != nil {
		log.Fatal("Could not open keywords.txt:", err)
	}

	// Parse lines, skip empty lines
	var keywords []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			keywords = append(keywords, line)
		}
	}

	if len(keywords) == 0 {
		log.Fatal("No keywords found in keywords.txt")
	}

	fmt.Printf("Loaded %d keywords: %v\n", len(keywords), keywords)

	jobs := make(chan string, len(keywords))
	var wg sync.WaitGroup

	file, _ := os.OpenFile("reachable_sites.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer file.Close()

	for w := 1; w <= MaxWorkers; w++ {
		wg.Add(1)
		go worker(jobs, &wg, file)
	}
	for _, kw := range keywords {
		jobs <- kw
	}
	close(jobs)
	wg.Wait()

	fmt.Println("\n✓ Done! Results saved to reachable_sites.txt")
}

func worker(jobs <-chan string, wg *sync.WaitGroup, logFile *os.File) {
	defer wg.Done()
	client := &http.Client{Timeout: 10 * time.Second}

	for kw := range jobs {
		searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(kw))

		for page := 1; page <= MaxPages; page++ {
			fmt.Printf("\n[Worker] Processing '%s' - Page %d\n", kw, page)

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
				log.Printf("Failed to fetch '%s': %v", kw, err)
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
				verifyAndLog(realURL, kw, client, logFile)
			})

			fmt.Printf("  [DEBUG] Found %d links on page %d\n", found, page)

			// Find next page URL for pagination
			nextURL := ""
			doc.Find("form[action='/html/'] input[name='s']").Each(func(_ int, s *goquery.Selection) {
				val, _ := s.Attr("value")
				nextURL = fmt.Sprintf(
					"https://html.duckduckgo.com/html/?q=%s&s=%s&dc=1&v=1&o=json&api=/d.js",
					url.QueryEscape(kw), val,
				)
			})

			if nextURL == "" {
				fmt.Printf("  No more pages for '%s'\n", kw)
				break
			}
			searchURL = nextURL
			time.Sleep(2 * time.Second)
		}
	}
}

func verifyAndLog(link string, kw string, client *http.Client, file *os.File) {
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
		file.WriteString(fmt.Sprintf("[%s] [%s] %s\n", kw, reason, link))
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
	file.WriteString(fmt.Sprintf("[%s] [%s | %d] %s\n", kw, label, status, link))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
