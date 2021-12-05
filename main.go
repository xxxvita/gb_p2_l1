package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type Crawler interface {
	Scan(ctx context.Context, url string, curDepth int)
	GetResultChan() <-chan CrawlResult
	Wait()
	Start()
}

type CrawlResult struct {
	Title string
	Url   string
	Err   error
}

type crawler struct {
	maxDepth   int
	req        Requester
	res        chan CrawlResult
	visited    map[string]struct{}
	visitedMu  sync.RWMutex
	maxDepthMu sync.RWMutex
}

func (c *crawler) GetResultChan() <-chan CrawlResult {
	return c.res
}

func (c *crawler) maxDepthInc(value int) {
	c.maxDepthMu.Lock()
	c.maxDepth += value
	c.maxDepthMu.Unlock()
}

func (c *crawler) MaxDepthGet() int {
	c.maxDepthMu.Lock()
	result := c.maxDepth
	c.maxDepthMu.Unlock()

	return result
}

func NewCrawler(maxDepth int, req Requester) *crawler {
	return &crawler{
		maxDepth: maxDepth,
		req:      req,
		res:      make(chan CrawlResult, 100),
		visited:  make(map[string]struct{}),
	}
}

func (c *crawler) Scan(ctx context.Context, url string, curDepth int) {
	c.visitedMu.RLock()
	if _, ok := c.visited[url]; ok {
		c.visitedMu.RUnlock()
		return
	}
	c.visitedMu.RUnlock()

	if curDepth >= c.MaxDepthGet() {
		return
	}

	select {
	case <-ctx.Done():
		return
	default:
		page, err := c.req.GetPage(ctx, url)

		c.visitedMu.Lock()
		c.visited[url] = struct{}{}
		c.visitedMu.Unlock()

		if err != nil {
			c.res <- CrawlResult{Url: url, Err: err}
			return
		}

		title := page.GetTitle()

		c.res <- CrawlResult{
			Title: title,
			Url:   url,
			Err:   nil,
		}

		links := page.GetLinks()
		for _, link := range links {
			go c.Scan(ctx, link, curDepth+1)
		}
	}
}

type Requester interface {
	GetPage(ctx context.Context, url string) (Page, error)
}

type reqWithDelay struct {
	delay time.Duration
	req   Requester
}

func NewRequestWithDelay(delay time.Duration, req Requester) *reqWithDelay {
	return &reqWithDelay{delay: delay, req: req}
}

func (r reqWithDelay) GetPage(ctx context.Context, url string) (Page, error) {
	time.Sleep(r.delay)
	return r.req.GetPage(ctx, url)
}

/*
type HttpClient interface {
	 Do(r *http.Request) (*http.Response, error)
}
*/

type requester struct {
	timeout time.Duration
}

func NewRequester(timeout time.Duration) *requester {
	return &requester{timeout: timeout}
}

func (r requester) GetPage(ctx context.Context, url string) (Page, error) {
	cl := &http.Client{
		Timeout: r.timeout,
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	rawPage, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer rawPage.Body.Close()

	return NewPage(rawPage.Body)
}

type Page interface {
	GetTitle() string
	GetLinks() []string
}

type page struct {
	doc *goquery.Document
}

func NewPage(raw io.Reader) (page, error) {
	doc, err := goquery.NewDocumentFromReader(raw)
	if err != nil {
		return page{}, err
	}

	return page{doc}, nil
}

func (p page) GetTitle() string {
	return p.doc.Find("title").First().Text()
}

func (p page) GetLinks() []string {
	var urls []string

	p.doc.Find("a").Each(func(_ int, s *goquery.Selection) {
		url, ok := s.Attr("href")
		if ok {
			//Здесь может быть относительная ссылка, нужно абсолютную
			urls = append(urls, url)
		}
	})

	return urls
}

const startUrl = "https://www.w3.org/Consortium/"

func processResult(ctx context.Context, in <-chan CrawlResult, cancel context.CancelFunc) {
	var errCount int

	for {
		select {
		case res := <-in:
			if res.Err != nil {
				errCount++

				fmt.Printf("ERROR Link: %s, err: %v\n", res.Url, res.Err)

				// if errCount >= 3 {
				// 	cancel()
				// }
			} else {
				fmt.Printf("Link: %s, Title: %s\n", res.Url, res.Title)
			}
		case <-ctx.Done():
			// Финализация работы по контексту с печатью не хороша
			fmt.Printf("context canceled (processResult)\n")
			return
		}
	}
}

func main() {
	var r Requester

	r = NewRequester(time.Minute)
	//r = NewRequestWithDelay(30*time.Second, r)
	// Без sync.WaitGroup и других механик, вынуждено ждём время для завершения работы
	// сканеров ссылок
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	crawler := NewCrawler(2, r)

	crawler.Scan(ctx, startUrl, 0)

	chanSignalTerminated := make(chan os.Signal, 2)
	signal.Notify(chanSignalTerminated, syscall.SIGTERM, syscall.SIGINT)

	// При сигнале SIGUSR1 увиличивается глубина поиска ссылок со страниц
	chanSignalUsr1 := make(chan os.Signal, 1)
	signal.Notify(chanSignalTerminated, syscall.SIGUSR1)

	go processResult(ctx, crawler.GetResultChan(), cancel)

	for {
		select {
		case <-chanSignalTerminated:
			fmt.Printf("Signal SIGTERM catched\n")
			cancel()
		case <-chanSignalUsr1:
			// Для будущих ссылок, учавствующие в процессе глубина поиска увеличивается на 1
			crawler.maxDepthInc(1)
		case <-ctx.Done():
			fmt.Printf("context canceled\n")
			return
		}
	}
}

func init() {
	pid := os.Getpid()
	fmt.Printf("My PID is: %d\n", pid)
}
