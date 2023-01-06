package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
)

type Result struct {
	Source string
	URL    string
}

var headers map[string]string

// Thread safe map
var sm sync.Map

func main() {
	urll := flag.String(("u"), "", "the url to crawl")
	threads := flag.Int("t", 8, "Number of threads to utilise.")
	depth := flag.Int("d", 2, "Depth to crawl.")
	maxSize := flag.Int("size", -1, "Page size limit, in KB.")
	insecure := flag.Bool("insecure", false, "Disable TLS verification.")
	subsInScope := flag.Bool("subs", false, "Include subdomains for crawling.")
	showJson := flag.Bool("json", false, "Output as JSON.")
	showSource := flag.Bool("s", false, "Show the source of URL based on where it was found. E.g. href, form, script, etc.")
	rawHeaders := flag.String(("h"), "", "Custom headers separated by two semi-colons. E.g. -h \"Cookie: foo=bar;;Referer: http://example.com/\" ")
	unique := flag.Bool(("unique"), false, "Show only unique urls.")
	proxy := flag.String(("proxy"), "", "Proxy URL. E.g. -proxy http://127.0.0.1:8080")
	timeout := flag.Int("timeout", -1, "Maximum time to crawl each URL from stdin, in seconds.")
	disableRedirects := flag.Bool("dr", false, "Disable following HTTP redirects.")

	flag.Parse()

	if *proxy != "" {
		os.Setenv("PROXY", *proxy)
	}
	proxyURL, _ := url.Parse(os.Getenv("PROXY"))

	// Convert the headers input to a usable map (or die trying)
	err := parseHeaders(*rawHeaders)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error parsing headers:", err)
		os.Exit(1)
	}

	// Check for stdin input
	// stat, _ := os.Stdin.Stat()
	// if (stat.Mode() & os.ModeCharDevice) != 0 {
	// 	fmt.Fprintln(os.Stderr, "No urls detected. Hint: cat urls.txt | hakrawler")
	// 	os.Exit(1)
	// }
	// var url *string
	results := make(chan string, *threads)
	go func() {
		// get each line of stdin, push it to the work channel
		// s := bufio.NewScanner(os.Stdin)
		// for s.Scan() {
		// url := s.Text()
		url := *urll
		hostname, err := extractHostname(url)
		if err != nil {
			log.Println("Error parsing URL:", err)
			return
		}

		allowed_domains := []string{hostname}
		// if "Host" header is set, append it to allowed domains
		if headers != nil {
			if val, ok := headers["Host"]; ok {
				allowed_domains = append(allowed_domains, val)
			}
		}

		// Instantiate default collector
		c := colly.NewCollector(
			// default user agent header
			colly.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/104.0.0.0 Safari/537.36"),
			// set custom headers
			colly.Headers(headers),
			// limit crawling to the domain of the specified URL
			colly.AllowedDomains(allowed_domains...),
			// set MaxDepth to the specified depth
			colly.MaxDepth(*depth),
			// specify Async for threading
			colly.Async(true),
		)

		// set a page size limit
		if *maxSize != -1 {
			c.MaxBodySize = *maxSize * 1024
		}

		// if -subs is present, use regex to filter out subdomains in scope.
		if *subsInScope {
			c.AllowedDomains = nil
			c.URLFilters = []*regexp.Regexp{regexp.MustCompile(".*(\\.|\\/\\/)" + strings.ReplaceAll(hostname, ".", "\\.") + "((#|\\/|\\?).*)?")}
		}

		// If `-dr` flag provided, do not follow HTTP redirects.
		if *disableRedirects {
			c.SetRedirectHandler(func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			})
		}
		// Set parallelism
		c.Limit(&colly.LimitRule{DomainGlob: "*", Parallelism: *threads})

		// Print every href found, and visit it
		c.OnHTML("a[href]", func(e *colly.HTMLElement) {
			link := e.Attr("href")
			printResult(link, "href", *showSource, *showJson, results, e)
			e.Request.Visit(link)
		})

		// find and print all the JavaScript files
		c.OnHTML("script[src]", func(e *colly.HTMLElement) {
			printResult(e.Attr("src"), "script", *showSource, *showJson, results, e)
		})

		// find and print all the form action URLs
		c.OnHTML("form[action]", func(e *colly.HTMLElement) {
			printResult(e.Attr("action"), "form", *showSource, *showJson, results, e)
		})

		// add the custom headers
		if headers != nil {
			c.OnRequest(func(r *colly.Request) {
				for header, value := range headers {
					r.Headers.Set(header, value)
				}
			})
		}

		if *proxy != "" {
			// Skip TLS verification for proxy, if -insecure specified
			c.WithTransport(&http.Transport{
				Proxy:           http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
			})
		} else {
			// Skip TLS verification if -insecure flag is present
			c.WithTransport(&http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
			})
		}

		if *timeout == -1 {
			// Start scraping
			c.Visit(url)
			// Wait until threads are finished
			c.Wait()
		} else {
			finished := make(chan int, 1)

			go func() {
				// Start scraping
				c.Visit(url)
				// Wait until threads are finished
				c.Wait()
				finished <- 0
			}()

			select {
			case _ = <-finished: // the crawling finished before the timeout
				close(finished)
				// continue
			case <-time.After(time.Duration(*timeout) * time.Second): // timeout reached
				log.Println("[timeout] " + url)
				// continue

			}
		}

		// }
		// if err := s.Err(); err != nil {
		// 	fmt.Fprintln(os.Stderr, "reading standard input:", err)
		// }
		close(results)
	}()

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	if *unique {
		for res := range results {
			if isUnique(res) {
				fmt.Fprintln(w, res)
			}
		}
	}
	for res := range results {
		fmt.Fprintln(w, res)
	}

}

// parseHeaders does validation of headers input and saves it to a formatted map.
func parseHeaders(rawHeaders string) error {
	if rawHeaders != "" {
		if !strings.Contains(rawHeaders, ":") {
			return errors.New("headers flag not formatted properly (no colon to separate header and value)")
		}

		headers = make(map[string]string)
		rawHeaders := strings.Split(rawHeaders, ";;")
		for _, header := range rawHeaders {
			var parts []string
			if strings.Contains(header, ": ") {
				parts = strings.SplitN(header, ": ", 2)
			} else if strings.Contains(header, ":") {
				parts = strings.SplitN(header, ":", 2)
			} else {
				continue
			}
			headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return nil
}

// extractHostname() extracts the hostname from a URL and returns it
func extractHostname(urlString string) (string, error) {
	u, err := url.Parse(urlString)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}

// print result constructs output lines and sends them to the results chan
func printResult(link string, sourceName string, showSource bool, showJson bool, results chan string, e *colly.HTMLElement) {
	result := e.Request.AbsoluteURL(link)
	if result != "" {
		if showJson {
			bytes, _ := json.Marshal(Result{
				Source: sourceName,
				URL:    result,
			})
			result = string(bytes)
		} else if showSource {
			result = "[" + sourceName + "] " + result
		}
		// If timeout occurs before goroutines are finished, recover from panic that may occur when attempting writing to results to closed results channel
		defer func() {
			if err := recover(); err != nil {
				return
			}
		}()
		results <- result
	}
}

// returns whether the supplied url is unique or not
func isUnique(url string) bool {
	_, present := sm.Load(url)
	if present {
		return false
	}
	sm.Store(url, true)
	return true
}
