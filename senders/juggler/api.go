package juggler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/combaine/combaine/common/chttp"
	"github.com/combaine/combaine/common/logger"
)

const (
	getChecksURL   = "http://%s/api/checks/checks?%s"
	updateCheckURL = "http://%s/api/checks/add_or_update?do=1"
	sendEventURL   = "http://%s/juggler-fcgi.py?%s"
	defaultTag     = "combaine"
)

type jugglerResponse map[ /*hostname*/ string]map[ /*serviceName*/ string]jugglerCheck

type jugglerChildrenCheck struct {
	Instance string `json:"instance"`
	Host     string `json:"host"`
	Type     string `json:"type"`
	Service  string `json:"service"`
}

type jugglerFlapConfig struct {
	Enable       int64 `codec:"enable" json:"-"`
	BoostTime    int64 `codec:"boost_time" json:"boost_time"`
	StableTime   int64 `codec:"stable_time" json:"stable_time"`
	CriticalTime int64 `codec:"critical_time" json:"critical_time"`
}

type jugglerCheck struct {
	Update           bool                   `json:"-"`
	Host             string                 `json:"host"`
	Service          string                 `json:"service"`
	Description      string                 `json:"description"`
	Aggregator       string                 `json:"aggregator"`
	AggregatorKWArgs aggKWArgs              `json:"aggregator_kwargs"`
	TTL              int                    `json:"ttl"`
	Tags             []string               `json:"tags"`
	Methods          []string               `json:"methods"`
	Children         []jugglerChildrenCheck `json:"children"`
	Flap             *jugglerFlapConfig     `json:"flaps,omitempty"`
	Namespace        string                 `json:"namespace"`
}

type aggKWArgs struct {
	IgnoreNoData int                      `codec:"ignore_nodata" json:"ignore_nodata"`
	Limits       []map[string]interface{} `codec:"limits,omitempty" json:"limits,omitempty"`
}

type jugglerEvent struct {
	taskTags    map[string]string
	Host        string   `json:"host"`
	Service     string   `json:"service"`
	Status      string   `json:"status"`
	Description string   `json:"description"`
	Instance    string   `json:"instance,omitempty"`
	Version     string   `json:"version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type jugglerBatchRequest struct {
	Events []jugglerEvent `json:"events"`
	Source string         `json:"source"`
}

type jugglerBatchResponse struct {
	Events []jugglerBatchEventReport `json:"events"`
	Error  *jugglerBatchError        `json:"error"`
}
type jugglerBatchEventReport struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}
type jugglerBatchError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
	Label   string `json:"label"`
}

// getCheck query juggler api for check
// and Unmarshal json response in to jugglerResponse type
func (js *Sender) getCheck(ctx context.Context) (jugglerResponse, error) {
	if len(js.Tags) == 0 {
		js.Tags = []string{"combaine"}
		logger.Debugf("%s Set query tags to default %s", js.id, js.Tags)
	}
	//do=1&include_children=true&tag_name=combaine&host_name={js.Host}
	query := url.Values{
		"do":               {"1"},
		"include_children": {"true"},
		"tag_name":         js.Tags,
		"host_name":        {js.Host},
	}.Encode()

	checkFetcher := func(ctx context.Context, id, q string, hosts []string) ([]byte, error) {
		var jerrors []error
	GET_CHECK:
		for _, jhost := range hosts {
			url := fmt.Sprintf(getChecksURL, jhost, q)
			logger.Infof("%s Query check %s", id, url)

			resp, err := chttp.Get(ctx, url)
			switch err {
			case nil:
				body, rerr := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				if rerr != nil {
					jerrors = append(jerrors, fmt.Errorf("%s: %s", jhost, rerr))
					continue
				}
				if resp.StatusCode != http.StatusOK {
					jerrors = append(jerrors, fmt.Errorf("%s: %v %s", jhost, resp.StatusCode, body))
					continue
				}
				logger.Debugf("%s Juggler response: %s", id, body)
				return body, nil
			case context.Canceled, context.DeadlineExceeded:
				jerrors = append(jerrors, err)
				break GET_CHECK
			default:
				jerrors = append(jerrors, fmt.Errorf("%s: %s", jhost, err))
				continue
			}
		}
		return nil, fmt.Errorf("Failed to get juggler check: %q", jerrors)
	}
	cJSON, err := GlobalCache.Get(ctx, js.Host, checkFetcher, js.id, query, js.JHosts)
	if err != nil {
		return nil, err
	}
	var hostChecks jugglerResponse
	if err := json.Unmarshal(cJSON, &hostChecks); err != nil {
		return nil, fmt.Errorf("Failed to Unmarshal hostChecks: %s", err)
	}
	var flap map[string]map[string]*jugglerFlapConfig
	if err := json.Unmarshal(cJSON, &flap); err != nil {
		return nil, fmt.Errorf("Failed to Unmarshal flaps: %s", err)
	}
	for c, v := range flap[js.Host] {
		if v.StableTime != 0 || v.CriticalTime != 0 || v.BoostTime != 0 {
			chk := hostChecks[js.Host][c]
			chk.Flap = v
			hostChecks[js.Host][c] = chk
		}
	}
	return hostChecks, nil
}

// ensureCheck check that juggler check exists and it in sync with task data
// if need it call add_or_update check
func (js *Sender) ensureCheck(ctx context.Context, hostChecks jugglerResponse, triggers []jugglerEvent) error {
	services, ok := hostChecks[js.Host]
	if !ok {
		logger.Debugf("%s Create new checks for %s", js.id, js.Host)
		services = make(map[string]jugglerCheck)
		hostChecks[js.Host] = services
	}
	childSet := make(map[string]struct{}) // set
	for serviceName, v := range services {
		for _, c := range v.Children {
			childSet[c.Host+":"+serviceName] = struct{}{}
		}
	}
	updated := make(map[string]struct{})
	for _, t := range triggers {
		check, ok := services[t.Service]
		if !ok {
			logger.Infof("%s Add new check %s.%s", js.id, js.Host, t.Service)
			check = jugglerCheck{Update: true}
		}

		// ensure check only once, but in if we can not
		// test metahost tag like t.Tags["type"] == "metahost"
		// because user may specify filter 'type: host'
		// and there will be triggers only for hosts
		if _, ok := updated[t.Service]; !ok {
			updated[t.Service] = struct{}{}
			logger.Infof("%s Ensure check %s for %s", js.id, t.Service, js.Host)

			// ttl
			js.ensureTTL(&check)
			// aggregator
			js.ensureAggregator(&check)
			// methods
			js.ensureMethods(&check)
			// flap
			js.ensureFlap(&check)
			// tags
			js.ensureTags(&check)
			// description
			js.ensureDescription(&check)
			// namespace
			js.ensureNamespace(&check)
		}

		// add children
		if _, ok := childSet[t.Host+":"+t.Service]; !ok {
			check.Update = true
			child := jugglerChildrenCheck{
				Host:    t.Host,
				Type:    "HOST", // FIXME? hardcode, delete?
				Service: t.Service,
			}
			logger.Debugf("%s Add children %s", js.id, child)
			check.Children = append(check.Children, child)
		}

		if check.Update {
			check.Host = js.Host
			check.Service = t.Service
			services[t.Service] = check
		}
	}
	cleanCache := false
	defer func() {
		// defer because cached item should be deleted after all check updated
		// or before return in case updateCheck ends with error
		if cleanCache {
			logger.Debugf("%s Clean cache for %s", js.id, js.Host)
			GlobalCache.Delete(js.Host)
		}
	}()
	for _, c := range services {
		if c.Update {
			cleanCache = true
			if err := js.updateCheck(ctx, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func (js *Sender) ensureTTL(c *jugglerCheck) {
	if js.TTL == 0 {
		js.TTL = 900 // default juggler ttl
	}
	if c.TTL != js.TTL {
		c.Update = true
		logger.Debugf("%s Check outdated, ttl differ: %d != %d", js.id, c.TTL, js.TTL)
		c.TTL = js.TTL
	}
}

func (js *Sender) ensureDescription(c *jugglerCheck) {
	if js.Description == "" {
		js.Description = js.CheckName
	}
	if c.Description != js.Description && js.Description != "" {
		c.Update = true
		logger.Debugf("%s Check outdated, description differ: '%s' != '%s'", js.id, c.Description, js.Description)
		c.Description = js.Description
	}
}

func (js *Sender) ensureAggregator(c *jugglerCheck) {
	aggregatorOutdated := false
	if c.AggregatorKWArgs.IgnoreNoData != js.AggregatorKWArgs.IgnoreNoData {
		aggregatorOutdated = true
	}
	if len(c.AggregatorKWArgs.Limits) != len(js.AggregatorKWArgs.Limits) {
		aggregatorOutdated = true
	}
	if !aggregatorOutdated {
	CHECK:
		for i, v := range js.AggregatorKWArgs.Limits {
			for k, jv := range v {
				if cv, ok := c.AggregatorKWArgs.Limits[i][k]; ok {
					if fmt.Sprintf("%v", cv) != fmt.Sprintf("%v", jv) {
						aggregatorOutdated = true
						break CHECK
					}
				} else {
					aggregatorOutdated = true
					break CHECK
				}
			}
		}

	}
	if aggregatorOutdated {
		c.Update = true
		logger.Debugf("%s Check outdated, aggregator differ: %s != %s", js.id, c.AggregatorKWArgs, js.AggregatorKWArgs)
		c.Aggregator = js.Aggregator
		c.AggregatorKWArgs = js.AggregatorKWArgs
	}
}

func (js *Sender) ensureMethods(c *jugglerCheck) {
	if len(js.Methods) == 0 {
		if js.Method != "" {
			js.Methods = strings.Split(js.Method, ",")
		} else {
			js.Methods = []string{"GOLEM"}
		}
	}
	methodsOutdated := false
	if len(c.Methods) != len(js.Methods) {
		methodsOutdated = true
	} else {
		checkMSet := make(map[string]struct{}, len(c.Methods))
		for _, m := range c.Methods {
			checkMSet[m] = struct{}{}
		}
		for _, m := range js.Methods {
			if _, ok := checkMSet[m]; !ok {
				methodsOutdated = true
				break
			}
		}
	}
	if methodsOutdated {
		c.Update = true
		logger.Debugf("%s Check outdated, METHODS differ: %s != %s", js.id, c.Methods, js.Methods)
		c.Methods = js.Methods
	}
}

func (js *Sender) ensureFlap(c *jugglerCheck) {
	if c.Flap == nil {
		c.Flap = &jugglerFlapConfig{}
	}
	c.Flap.Enable = 1 // enable field not set by json, set in manually

	if f, ok := js.ChecksOptions[c.Service]; ok {
		if f.Enable == 1 {
			if *c.Flap != f {
				c.Update = true
				logger.Debugf("%s Check outdated, Flap differ: %s != %s", js.id, c.Flap, js.Flap)
				c.Flap = &f
			}
		}
	} else {
		// if flap setting not set for check individually, try apply global settings
		if js.Flap != nil && js.Flap.Enable == 1 {
			if *c.Flap != *js.Flap {
				c.Update = true
				logger.Debugf("%s Check outdated, Flap differ: %s != %s", js.id, c.Flap, js.Flap)
				c.Flap = js.Flap
			}
		} else {
			c.Flap = nil
		}
	}
}

func (js *Sender) ensureTags(c *jugglerCheck) {
	if len(js.Tags) == 0 {
		js.Tags = []string{defaultTag}
	}
	// TODO: tags by servces in juggler config as for flaps?
	if c.Tags == nil || len(c.Tags) == 0 {
		c.Update = true
		logger.Debugf("%s Check outdated, Tags differ: %s != %s", js.id, c.Tags, js.Tags)
		c.Tags = make([]string, len(js.Tags))
		copy(c.Tags, js.Tags)
	} else {
		tagsSet := make(map[string]struct{}, len(c.Tags))
		for _, tag := range c.Tags {
			tagsSet[tag] = struct{}{}
		}
		for _, tag := range js.Tags {
			if _, ok := tagsSet[tag]; !ok {
				c.Update = true
				logger.Infof("%s Add tag %s", js.id, tag)
				c.Tags = append(c.Tags, tag)
			}
		}
	}
}

func (js *Sender) ensureNamespace(c *jugglerCheck) {
	if js.Namespace == "" {
		js.Namespace = "combaine"
	}
	if js.Config.Token != "" && c.Namespace != js.Namespace {
		c.Update = true
		logger.Debugf("%s Check outdated, namespace differ: '%s' != '%s'", js.id, c.Namespace, js.Namespace)
		c.Namespace = js.Namespace
	}
}

func (js *Sender) updateCheck(ctx context.Context, check jugglerCheck) error {
	logger.Infof("%s Update check %s for %s", js.id, check.Service, check.Host)

	cJSON, err := json.Marshal(check)
	if err != nil {
		return err
	}
	logger.Debugf("%s JSON payload for updating check: %s", js.id, cJSON)

	errs := make(map[string]string, 0)
	for _, host := range js.JHosts {
		url := fmt.Sprintf(updateCheckURL, host)
		req, err := http.NewRequest("POST", url, bytes.NewReader(cJSON))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if js.Config.Token != "" {
			req.Header.Set("Authorization", "OAuth "+js.Config.Token)
		}
		resp, err := chttp.Do(ctx, req.WithContext(ctx))
		switch err {
		case nil:
			body, err := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				logger.Errf("%s %s", js.id, err)
				errs[err.Error()] = ""
				continue
			}

			if resp.StatusCode != http.StatusOK {
				logger.Warnf("%s Update check query %s: %d - %s", js.id, url, resp.StatusCode, body)
				errs[string(body)] = ""
				continue
			}
			logger.Infof("%s Sucessfully send update %s.%s %s: %s", js.id, check.Host, check.Service, url, body)
			return nil
		case context.Canceled, context.DeadlineExceeded:
			logger.Errf("%s %s", js.id, err)
			return err
		default:
			logger.Errf("%s %s", js.id, err)
			errs[err.Error()] = ""
			continue
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", errs)
	}
	logger.Errf("%s failed to sent update check for %v", js.id, check)
	return fmt.Errorf("Upexpected error, can't update check for %s.%s", check.Host, check.Service)
}
