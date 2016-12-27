package juggler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	yaml "gopkg.in/yaml.v2"

	"github.com/combaine/combaine/common/tasks"
	"github.com/stretchr/testify/assert"
)

var data []tasks.AggregationResult
var ts *httptest.Server

func DefaultJugglerTestConfig() *Config {
	conf := DefaultConfig()
	// add test conditions
	conf.OK = []string{"${nginx}.get('5xx', 0)<0.060"}
	conf.INFO = []string{"${nginx}.get('5xx', 0)<0.260"}
	//conf.WARN = []string{"${nginx}.get('5xx', 0)<0.460"}
	conf.CRIT = []string{"${nginx}.get('5xx', 0)>1.060"}
	// add test config
	conf.JPluginConfig = map[string]interface{}{
		"checks": map[string]interface{}{
			"testTimings": map[string]interface{}{
				"query":  "%S+/%S+/%S+timings/7",
				"status": "WARN",
				"limit":  0.900, // second
			},
			"test4xx": map[string]interface{}{
				"query":  "%S+/%S+/4xx",
				"status": "CRIT",
				"limit":  30,
			},
		},
	}

	testConf := "testdata/config/juggler_example.yaml"
	os.Setenv("JUGGLER_CONFIG", testConf)
	sConf, err := GetSenderConfig()
	if err != nil {
		panic(fmt.Sprintf("Failed to load juggler sender config %s: %s", testConf, err))
	}

	conf.PluginsDir = sConf.PluginsDir
	conf.JHosts = sConf.Hosts
	conf.JFrontend = sConf.Frontend

	return conf
}

// Benchmarks
func BenchmarkDataToLuaTable(b *testing.B) {
	l, err := LoadPlugin("Test Id", "testdata/plugins", "test")
	if err != nil {
		panic(err)
	}
	for i := 0; i < b.N; i++ {
		table, err := dataToLuaTable(l, data)
		if err != nil {
			b.Fatal(err)
		}
		l.SetGlobal("table", table)
		l.Push(l.GetGlobal("sumTable"))
		l.Push(l.GetGlobal("table"))
		l.Call(1, 1)
		l.Pop(1)
	}
	l.Close()
}

// Tests

func TestGetJugglerSenderConfig(t *testing.T) {
	testConf := "testdata/config/nonExistingJugglerConfig.yaml"
	os.Setenv("JUGGLER_CONFIG", testConf)
	conf, err := GetSenderConfig()
	assert.Error(t, err)

	testConf = "testdata/config/juggler_example.yaml"
	os.Setenv("JUGGLER_CONFIG", testConf)
	conf, err = GetSenderConfig()
	assert.Equal(t, conf.Hosts[0], "host1")

	testConf = "testdata/config/without_fronts.yaml"
	os.Setenv("JUGGLER_CONFIG", testConf)
	conf, err = GetSenderConfig()
	assert.Equal(t, conf.Frontend, conf.Hosts)
	assert.Equal(t, conf.PluginsDir, defaultPluginsDir)

}

func TestLoadPlugin(t *testing.T) {
	if _, err := LoadPlugin("Test Id", ".", "file_not_exists.lua"); err == nil {
		t.Fatalf("Loading non existing plugin should return error")
	}
	if _, err := LoadPlugin("Test Id", "testdata/plugins", "test"); err != nil {
		t.Fatalf("Failed to load plugin 'test': %s", err)
	}
}

func TestPrepareLuaEnv(t *testing.T) {
	jconf := DefaultJugglerTestConfig()
	jconf.Plugin = "test"

	l, err := LoadPlugin("Test Id", jconf.PluginsDir, jconf.Plugin)
	assert.NoError(t, err)
	js, err := NewSender(jconf, "Test ID")
	assert.NoError(t, err)

	js.state = l
	js.preparePluginEnv(data)

	l.Push(l.GetGlobal("testEnv"))
	l.Call(0, 1)

	result := l.ToString(1)
	assert.Equal(t, "OK", result)
}

func TestRunPlugin(t *testing.T) {
	jconf := DefaultJugglerTestConfig()

	js, err := NewSender(jconf, "Test ID")
	assert.NoError(t, err)

	jconf.Plugin = "correct"
	l, err := LoadPlugin("Test Id", jconf.PluginsDir, jconf.Plugin)
	assert.NoError(t, err)
	js.state = l
	assert.NoError(t, js.preparePluginEnv(data))

	_, err = js.runPlugin()
	assert.NoError(t, err)

	jconf.Plugin = "incorrect"
	l, err = LoadPlugin("Test Id", jconf.PluginsDir, jconf.Plugin)
	assert.NoError(t, err)
	js.state = l
	assert.NoError(t, js.preparePluginEnv(data))
	_, err = js.runPlugin()
	if err == nil {
		err = errors.New("incorrect should fail")
	}
	assert.Contains(t, err.Error(), "Expected 'run' function inside plugin")
}

func TestQueryLuaTable(t *testing.T) {
	jconf := DefaultJugglerTestConfig()

	js, err := NewSender(jconf, "Test ID")
	assert.NoError(t, err)

	jconf.Plugin = "test"
	l, err := LoadPlugin("Test Id", jconf.PluginsDir, jconf.Plugin)
	assert.NoError(t, err)
	js.state = l
	assert.NoError(t, js.preparePluginEnv(data))
	l.Push(l.GetGlobal("testQuery"))
	l.Call(0, 1)
	result := l.ToTable(1)

	events, err := js.luaResultToJugglerEvents(result)
	t.Logf("Test events: %v", events)
	assert.NoError(t, err)
	assert.Len(t, events, 32)
}

func TestGetCheck(t *testing.T) {
	jconf := DefaultJugglerTestConfig()
	jconf.JHosts = []string{"localhost:3333"}
	jconf.JFrontend = []string{ts.Listener.Addr().String()}

	js, err := NewSender(jconf, "Test ID")
	assert.NoError(t, err)
	_, err = js.getCheck(context.TODO())
	assert.Error(t, err)

	jconf.JHosts = []string{"localhost:3333", ts.Listener.Addr().String()}
	js, err = NewSender(jconf, "Test ID")
	cases := []struct {
		name      string
		exists    bool
		len       int
		withFlaps map[string]*jugglerFlapConfig
	}{
		{"hostname_from_config", true, 5, map[string]*jugglerFlapConfig{
			"type1_timings":  nil,
			"type2_timings":  {StableTime: 60, CriticalTime: 90},
			"prod-app_5xx":   nil,
			"common_log_err": nil,
			"api_5xx":        nil,
		}},
		{"frontend", true, 4, map[string]*jugglerFlapConfig{
			"upstream_timings":      nil,
			"ssl_handshake_timings": {StableTime: 60, CriticalTime: 90},
			"4xx": nil,
			"2xx": nil,
		}},
		{"nonExisting", false, 0, make(map[string]*jugglerFlapConfig)},
	}

	ctx := context.TODO()
	for _, c := range cases {
		js.Host = c.name
		jugglerResp, err := js.getCheck(ctx)
		if c.exists {
			assert.NoError(t, err)
		} else {
			assert.Contains(t, fmt.Sprintf("%v", err), "no such file")
		}
		assert.Len(t, jugglerResp[js.Host], c.len)

		for k, v := range c.withFlaps {
			assert.Equal(t, v, jugglerResp[c.name][k].Flap)
		}
	}
}

func TestEnsureCheck(t *testing.T) {
	cases := []struct {
		name string
		tags map[string][]string
	}{
		{"hostname_from_config", map[string][]string{
			"type2_timings":  {"app", "combaine"},
			"common_log_err": {"common", "combaine"},
		}},
		{"frontend", map[string][]string{
			"ssl_handshake_timings": {"app", "front", "core", "combaine"},
			"4xx": {"combaine"},
		}},
	}

	jconf := DefaultJugglerTestConfig()
	jconf.Flap = &jugglerFlapConfig{Enable: 1, StableTime: 60}
	jconf.JPluginConfig = map[string]interface{}{
		"checks": map[string]interface{}{
			"testTimings": map[string]interface{}{
				"type":       "metahost",
				"query":      ".+_timings$",
				"percentile": 6, // 97
				"status":     "WARN",
				"limit":      0.900, // second
			},
			"testErr": map[string]interface{}{
				"type":   "metahost",
				"query":  "[4e][xr][xr]$",
				"status": "CRIT",
				"limit":  30,
			},
		},
	}

	jconf.JHosts = []string{ts.Listener.Addr().String()}
	jconf.JFrontend = []string{ts.Listener.Addr().String()}
	jconf.Plugin = "test_ensure_check"

	js, err := NewSender(jconf, "Test ID")
	assert.NoError(t, err)

	state, err := LoadPlugin("Test Id", js.PluginsDir, js.Plugin)
	assert.NoError(t, err)
	js.state = state
	assert.NoError(t, js.preparePluginEnv(data))

	jEvents, err := js.runPlugin()
	assert.NoError(t, err)
	t.Logf("juggler events: %v", jEvents)

	ctx := context.TODO()
	for _, c := range cases {
		js.Host = c.name
		checks, err := js.getCheck(ctx)
		assert.NoError(t, err)
		checks["nonExistingHost"] = map[string]jugglerCheck{"nonExistingCheck": {}}
		assert.NoError(t, js.ensureCheck(ctx, checks, jEvents))
		js.Tags = []string{} // reset tags here for coverage purpose
		for service, tags := range c.tags {
			assert.Equal(t, tags, checks[c.name][service].Tags, fmt.Sprintf("host %s servce %s", c.name, service))
		}
	}
	// non existing check check
	js.Host = "someHost"
	checks := map[string]map[string]jugglerCheck{"nonExistingHost": {
		"nonExistingCheck": jugglerCheck{},
	}}
	assert.NoError(t, js.ensureCheck(ctx, checks, jEvents))
}

func TestSendEvent(t *testing.T) {
	jconf := DefaultJugglerTestConfig()

	jconf.Aggregator = "timed_more_than_limit_is_problem"
	jconf.AggregatorKWArgs = aggKWArgs{IgnoreNoData: 1,
		Limits: []map[string]interface{}{
			{"crit": "146%", "day_start": 1, "day_end": 7, "time_start": 20, "time_end": 8},
		}}

	jconf.JPluginConfig = map[string]interface{}{
		"checks": map[string]interface{}{
			"testTimings": map[string]interface{}{
				"type":       "metahost",
				"query":      ".+_timings$",
				"percentile": 6, // 97
				"status":     "WARN",
				"limit":      0.900, // second
			},
			"testErr": map[string]interface{}{
				"type":   "metahost",
				"query":  "[4e][xr][xr]$",
				"status": "CRIT",
				"limit":  30,
			},
		},
	}
	jconf.JHosts = []string{"localhost:3333", ts.Listener.Addr().String()}
	jconf.JFrontend = []string{"localhost:3333", ts.Listener.Addr().String()}
	jconf.Plugin = "test_ensure_check"

	cases := []string{"hostname_from_config", "deadline", "frontend"}
	for _, c := range cases {
		jconf.Host = c
		if c == "deadline" {
			js, err := NewSender(jconf, "Test ID")
			assert.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 1)
			assert.Contains(t, fmt.Sprintf("%s", js.Send(ctx, data)), context.DeadlineExceeded.Error())
			cancel()
		} else {
			js, err := NewSender(jconf, "Test ID")
			assert.NoError(t, err)
			err = js.Send(context.TODO(), data)
			//assert.Contains(t, fmt.Sprintf("%s", err), "getsockopt: connection refused")
			assert.Contains(t, fmt.Sprintf("%s", err), "failed to send 6/12 events")
		}
	}
}

func TestMain(m *testing.M) {
	dataYaml, yerr := ioutil.ReadFile("testdata/payload.yaml")
	if yerr != nil {
		panic(yerr)
	}
	//var data []tasks.AggregationResult is global
	if yerr := yaml.Unmarshal([]byte(dataYaml), &data); yerr != nil {
		panic(yerr)
	}

	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/checks/checks":
			hostName := r.URL.Query().Get("host_name")
			if hostName == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintln(w, "Query parameter host_name not specified")
				return
			}
			fileName := fmt.Sprintf("testdata/checks/%s.json", hostName)
			resp, err := ioutil.ReadFile(fileName)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "Failed to read file %s, %s", fileName, err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(resp)
		case "/api/checks/add_or_update":
			reqBytes, err := ioutil.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(500)
				fmt.Fprintln(w, err)
			}
			w.WriteHeader(200)
			io.Copy(w, bytes.NewReader(reqBytes))
			//fmt.Fprintln(w, reqJSON)
		case "/juggler-fcgi.py":
			fmt.Fprintln(w, "OK")
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, "Not Found")
		}
	}))
	defer ts.Close()

	os.Exit(m.Run())
}