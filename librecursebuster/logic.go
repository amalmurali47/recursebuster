package librecursebuster

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
)

func ManageRequests(cfg Config, state State, wg *sync.WaitGroup, pages, newPages, confirmed chan SpiderPage, workers chan struct{}, printChan chan OutLine, maxDirs chan struct{}) {
	//manages net request workers
	for {
		page := <-pages
		if state.Blacklist[page.Url] {
			wg.Done()
			printChan <- OutLine{
				Content: fmt.Sprintf("Not testing blacklisted URL: %s", page.Url),
				Type:    Info,
			}
			continue
		}

		workers <- struct{}{}
		wg.Add(1)
		go testURL(cfg, state, wg, page.Url, state.Client, page.Depth, newPages, workers, confirmed, printChan)

		if cfg.Wordlist != "" && string(page.Url[len(page.Url)-1]) == "/" { //if we are testing a directory

			//check for wildcard response

			wg.Add(1)
			go dirBust(cfg, state, page, wg, workers, pages, newPages, confirmed, printChan, maxDirs)
		}
		wg.Done()
	}
}

func ManageNewURLs(cfg Config, state State, wg *sync.WaitGroup, pages, newpages chan SpiderPage, printChan chan OutLine) {
	//decides on whether to add to the directory list, or add to file output
	checked := make(map[string]bool)
	preCheck := make(map[string]bool)
	for {
		candidate := <-newpages

		//shortcut (will make checked much bigger than it should be, but will save cycles)
		if _, ok := preCheck[candidate.Url]; ok {
			wg.Done()
			continue
		}
		preCheck[candidate.Url] = true

		u, err := url.Parse(strings.TrimSpace(candidate.Url))

		if err != nil {
			wg.Done()
			printChan <- OutLine{
				Content: fmt.Sprintf("URL Parse Failed: %s %s", candidate.Url, err),
				Type:    Error,
			}
			continue //probably a better way of doing this
		}

		if len(u.Host) == 0 {
			u.Host = state.ParsedURL.Host
		}

		actualUrl := state.ParsedURL.Scheme + "://" + u.Host

		//path.Clean removes trailing /, so we need to add it in again after cleaning (removing dots etc) :rolling eyes emoji:
		var didHaveSlash bool
		if len(u.Path) > 0 {
			didHaveSlash = string(u.Path[len(u.Path)-1]) == "/"
		}

		if len(u.Path) > 0 && string(u.Path[0]) != "/" {
			u.Path = "/" + u.Path
		}

		cleaned := path.Clean(u.Path)

		if string(cleaned[0]) != "/" {
			cleaned = "/" + cleaned
		}
		if cleaned != "." {
			actualUrl += cleaned

		}
		if didHaveSlash && cleaned != "/" {
			actualUrl += "/"
		}

		if _, ok := checked[actualUrl]; !ok && //must have not checked it before
			(u.Host == state.ParsedURL.Host || state.Whitelist[u.Host]) { // && //must be within same domain, or be in whitelist

			checked[actualUrl] = true

			wg.Add(1)
			pages <- SpiderPage{Url: actualUrl}

			//also add any directories in the supplied path to the 'to be hacked' queue
			path := ""
			dirs := strings.Split(u.Path, "/")
			for i, y := range dirs {
				//newPage := librecursebuster.SpiderPage{}
				path = path + y
				if len(path) > 0 && string(path[len(path)-1]) != "/" && i != len(dirs)-1 {
					path = path + "/" //don't add double /'s, and don't add on the last value
				}
				//prepend / if it doesn't already exist
				if len(path) > 0 && string(path[0]) != "/" {
					path = "/" + path
				}
				newDir := state.ParsedURL.Scheme + "://" + state.ParsedURL.Host + path
				newPage := SpiderPage{}
				newPage.Url = newDir
				wg.Add(1)
				newpages <- newPage
			}
		}

		wg.Done()
	}
}

func testURL(cfg Config, state State, wg *sync.WaitGroup, urlString string, client *http.Client,
	depth int, newPages chan SpiderPage, workers chan struct{},
	confirmedGood chan SpiderPage, printChan chan OutLine) {
	defer func() {
		wg.Done()
		atomic.AddUint64(state.TotalTested, 1)
	}()

	headResp, content, good := evaluateURL(cfg, state, urlString, client, workers, printChan)

	if !good && !cfg.ShowAll {
		return
	}

	//inception (but also check if the directory version is good, and add that to the queue too)
	if string(urlString[len(urlString)-1]) != "/" && good {
		wg.Add(1)
		newPages <- SpiderPage{Url: urlString + "/"}
	}

	wg.Add(1)
	confirmedGood <- SpiderPage{Url: urlString, Depth: depth, Result: headResp}

	if !cfg.NoSpider && good {
		urls := getUrls(content, printChan)
		for _, x := range urls { //add any found pages into the pool
			//add all the directories
			newPage := SpiderPage{}
			newPage.Url = x

			if cfg.Debug {
				printChan <- OutLine{
					Content: fmt.Sprintf("Found URL on page: %s", x),
					Type:    Debug,
				}
			}
			wg.Add(1)
			newPages <- newPage
		}
	}
}

func dirBust(cfg Config, state State, page SpiderPage, wg *sync.WaitGroup, workers chan struct{}, pages, newPages, confirmed chan SpiderPage, printChan chan OutLine, maxDirs chan struct{}) {
	defer wg.Done()

	//check to make sure we aren't dirbusting a wildcardyboi
	workers <- struct{}{}
	_, _, res := evaluateURL(cfg, state, page.Url+RandString(printChan), state.Client, workers, printChan)

	if res { //true response indicates a good response for a guid path, unlikely good
		printChan <- OutLine{
			Content: fmt.Sprintf("Wildcard repsonse detected, skipping dirbusting of %s", page.Url),
			Type:    Info,
		}
		return
	}

	//load in the wordlist to a channel (can probs be async)
	wordsChan := make(chan string, 300) //don't expect we will need it much bigger than this

	go LoadWords(cfg.Wordlist, wordsChan, printChan) //wordlist management doesn't need waitgroups, because of the following range statement

	maxDirs <- struct{}{}
	printChan <- OutLine{
		Content: fmt.Sprintf("Dirbusting %s", page.Url),
		Type:    Info,
	}
	for word := range wordsChan { //will receive from the channel until it's closed
		//read words off the channel, and test it

		//test with as many spare threads as we can
		workers <- struct{}{}
		//	localWG.Add(1)
		wg.Add(1)
		if len(state.Extensions) > 0 && state.Extensions[0] != "" {
			for _, ext := range state.Extensions {
				go testURL(cfg, state, wg, page.Url+word+"."+ext, state.Client, page.Depth, newPages, workers, confirmed, printChan)
			}
		} else {
			go testURL(cfg, state, wg, page.Url+word, state.Client, page.Depth, newPages, workers, confirmed, printChan)
		}
	}
	<-maxDirs
	printChan <- OutLine{
		Content: fmt.Sprintf("Finished dirbusting: %s", page.Url),
		Type:    Info,
	}

}
