package gfwlist

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type hostWildcardRule struct {
	pattern string
}

func (r *hostWildcardRule) match(req *http.Request) bool {
	if strings.Contains(req.Host, r.pattern) {
		return true
	}
	return false
}

type urlWildcardRule struct {
	pattern     string
	prefixMatch bool
}

func (r *urlWildcardRule) match(req *http.Request) bool {
	if len(req.URL.Scheme) == 0 {
		req.URL.Scheme = "https"
	}
	if r.prefixMatch {
		return strings.HasPrefix(req.URL.String(), r.pattern)
	}
	return strings.Contains(req.URL.String(), r.pattern)
}

type regexRule struct {
	pattern string
}

func (r *regexRule) match(req *http.Request) bool {
	if len(req.URL.Scheme) == 0 {
		req.URL.Scheme = "https"
	}
	matched, err := regexp.MatchString(r.pattern, req.URL.String())
	if nil != err {
		log.Printf("Invalid regex pattern:%s wiuth reason:%v", r.pattern, err)
	}
	return matched
}

type whiteListRule struct {
	r gfwListRule
}

func (r *whiteListRule) match(req *http.Request) bool {
	return r.r.match(req)
}

type gfwListRule interface {
	match(req *http.Request) bool
}

type GFWList struct {
	ruleList []gfwListRule
	mutex    sync.Mutex
}

func (gfw *GFWList) clone(n *GFWList) {
	gfw.mutex.Lock()
	defer gfw.mutex.Unlock()
	gfw.ruleList = n.ruleList
}

func (gfw *GFWList) IsBlockedByGFW(req *http.Request) bool {
	gfw.mutex.Lock()
	defer gfw.mutex.Unlock()
	for _, rule := range gfw.ruleList {
		if rule.match(req) {
			if _, ok := rule.(*whiteListRule); ok {
				//log.Printf("#### %s is in whilte list %v", req.Host, rule.(*whiteListRule).r)
				return false
			}
			return true
		}
	}
	return false
}

// func replaceRegexChars(rule string) string {
// 	rule = strings.TrimSpace(rule)
// 	rule = strings.Replace(rule, ".", "\\.", -1)
// 	rule = strings.Replace(rule, "?", "\\?", -1)
// 	rule = strings.Replace(rule, "*", ".*", -1)
// 	return rule
// }

func Parse(rules string) (*GFWList, error) {
	reader := bufio.NewReader(strings.NewReader(rules))
	gfw := new(GFWList)
	//i := 0
	for {
		line, _, err := reader.ReadLine()
		if nil != err {
			break
		}
		str := strings.TrimSpace(string(line))
		//comment
		if strings.HasPrefix(str, "!") || len(str) == 0 || strings.HasPrefix(str, "[") {
			continue
		}
		var rule gfwListRule
		isWhileListRule := false
		if strings.HasPrefix(str, "@@") {
			str = str[2:]
			isWhileListRule = true
		}
		if strings.HasPrefix(str, "/") && strings.HasSuffix(str, "/") {
			str = str[1 : len(str)-1]
			rule = &regexRule{str}
		} else {
			if strings.HasPrefix(str, "||") {
				rule = &hostWildcardRule{str[2:]}
			} else if strings.HasPrefix(str, "|") {
				rule = &urlWildcardRule{str[1:], true}
			} else {
				rule = &urlWildcardRule{str, false}
			}
		}
		if isWhileListRule {
			rule = &whiteListRule{rule}
		}
		gfw.ruleList = append(gfw.ruleList, rule)
	}
	return gfw, nil
}

func ParseRaw(rules string) (*GFWList, error) {
	content, err := base64.StdEncoding.DecodeString(string(rules))
	if err != nil {
		return nil, err
	}
	return Parse(string(content))
}

func NewGFWList(u string, proxy string, userRules []string, cacheFile string, watch bool) (*GFWList, error) {
	hc := &http.Client{}
	if len(proxy) > 0 {
		hc.Transport = &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse(proxy)
			},
		}
	}
	nextFetchTime := 6 * time.Hour
	firstFetch := true
	fetch := func() (string, error) {
		var gfwlistContent string
		fetchFromRemote := false
		if firstFetch {
			if len(cacheFile) > 0 {
				if _, err := os.Stat(cacheFile); nil == err {
					gfwlistBody, _ := ioutil.ReadFile(cacheFile)
					gfwlistContent = string(gfwlistBody)
					nextFetchTime = 30 * time.Second
				}
			}
		}
		if len(gfwlistContent) == 0 || !firstFetch {
			firstFetch = false
			resp, err := hc.Get(u)
			if nil != err {
				return "", err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return "", fmt.Errorf("Invalid response:%v", resp)
			}
			body, err := ioutil.ReadAll(resp.Body)
			if nil != err {
				return "", err
			}
			plainTxt, err := base64.StdEncoding.DecodeString(string(body))
			if nil != err {
				return "", err
			}
			fetchFromRemote = true
			gfwlistContent = string(plainTxt)
			if len(userRules) > 0 {
				userGfwlistContent := "\n!################User Rule List Begin#################\n"
				for _, rule := range userRules {
					userGfwlistContent = userGfwlistContent + rule + "\n"
				}
				userGfwlistContent = userGfwlistContent + "!################User Rule List End#################\n"
				gfwlistContent = gfwlistContent + userGfwlistContent
			}
		}
		if len(cacheFile) > 0 && fetchFromRemote {
			ioutil.WriteFile(cacheFile, []byte(gfwlistContent), 0666)
		}
		return gfwlistContent, nil
	}
	content, err := fetch()
	if nil != err {
		return nil, err
	}
	gfwlist, err := Parse(content)
	if nil != err {
		return nil, err
	}
	if watch {
		go func() {
			for {
				select {
				case <-time.After(nextFetchTime):
					newContent, nerr := fetch()
					if nerr == nil {
						nlist, _ := Parse(newContent)
						gfwlist.clone(nlist)
					}
				}
			}
		}()
	}
	return gfwlist, nil
}
