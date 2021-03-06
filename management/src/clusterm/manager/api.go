package manager

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/contiv/cluster/management/src/configuration"
	"github.com/contiv/cluster/management/src/monitor"
	"github.com/contiv/errored"
	"github.com/gorilla/mux"
)

// MonitorNode contains the info about a node in monitor event.
type MonitorNode struct {
	Label    string `json:"label"`
	Serial   string `json:"serial"`
	MgmtAddr string `json:"addr"`
}

// MonitorEvent wraps the info about monitor event type and respective nodes
type MonitorEvent struct {
	Name  string        `json:"name"`
	Nodes []MonitorNode `json:"nodes"`
}

// APIRequest is the general request body expected by clusterm from it's client
type APIRequest struct {
	Nodes     []string     `json:"nodes,omitempty"`
	Addrs     []string     `json:"addrs,omitempty"`
	HostGroup string       `json:"host_group,omitempty"`
	ExtraVars string       `json:"extra_vars,omitempty"`
	Job       string       `json:"job,omitempty"`
	Event     MonitorEvent `json:"monitor_event,omitempty"`
	Config    *Config      `json:"config,omitempty"`
}

// errInvalidJSON is the error returned when an invalid json value is specified for
// the ansible extra variables configuration
func errInvalidJSON(name string, err error) error {
	return errored.Errorf("%q should be a valid json. Error: %s", name, err)
}

// errJobNotExist is the error returned when a job with specified label doesn't exists
func errJobNotExist(job string) error {
	return errored.Errorf("info for %q job doesn't exist", job)
}

// errInvalidJobLabel is the error returned when an invalid or empty job label
// is specified as part of job info request
func errInvalidJobLabel(job string) error {
	return errored.Errorf("Invalid or empty job label specified: %q", job)
}

// errInvalidEventName is the error returned when an invalid or empty event name
// is specified as part of monitor event request
func errInvalidEventName(event string) error {
	return errored.Errorf("Invalid or empty event name specified: %q", event)
}

// errNilConfig is the error returned when a nil configuration value is
// specified as part of clusterm configuration update request
func errNilConfig() error {
	return errored.Errorf("nil value specified for clusterm configuration")
}

func (m *Manager) apiLoop(servingCh chan struct{}) error {
	//set following headers for requests expecting a body
	jsonContentHdrs := []string{"Content-Type", "application/json"}
	//set following headers for requests that don't expect a body like get node info.
	emptyHdrs := []string{}
	reqs := map[string][]struct {
		url  string
		hdrs []string
		hdlr http.HandlerFunc
	}{
		"GET": {
			{"/" + getNodeInfo, emptyHdrs, get(m.oneNode)},
			{"/" + GetNodesInfo, emptyHdrs, get(m.allNodes)},
			{"/" + GetGlobals, emptyHdrs, get(m.globalsGet)},
			{"/" + getJob, emptyHdrs, get(m.jobGet)},
			{"/" + getJobLog, emptyHdrs, get(m.logsGet)},
			{"/" + GetPostConfig, emptyHdrs, get(m.configGet)},
			{"/" + getDebugPrefix + "/", emptyHdrs, pprof.Index},
			{"/" + getDebugPrefix + "/cmdline", emptyHdrs, pprof.Cmdline},
			{"/" + getDebugPrefix + "/profile", emptyHdrs, pprof.Profile},
			{"/" + getDebugPrefix + "/symbol", emptyHdrs, pprof.Symbol},
			{"/" + getDebugPrefix + "/trace", emptyHdrs, pprof.Trace},
			{"/" + getDebug, emptyHdrs, pprof.Index},
		},
		"POST": {
			{"/" + PostNodesCommission, jsonContentHdrs, post(m.nodesCommission)},
			{"/" + PostNodesDecommission, jsonContentHdrs, post(m.nodesDecommission)},
			{"/" + PostNodesUpdate, jsonContentHdrs, post(m.nodesUpdate)},
			{"/" + PostNodesDiscover, jsonContentHdrs, post(m.nodesDiscover)},
			{"/" + PostGlobals, jsonContentHdrs, post(m.globalsSet)},
			{"/" + PostMonitorEvent, jsonContentHdrs, post(m.monitorEvent)},
			{"/" + GetPostConfig, jsonContentHdrs, post(m.configSet)},
		},
	}

	r := mux.NewRouter()
	for method, items := range reqs {
		for _, item := range items {
			r.Headers(item.hdrs...).Path(item.url).Methods(method).HandlerFunc(item.hdlr)
		}
	}

	l, err := net.Listen("tcp", m.addr)
	if err != nil {
		logrus.Errorf("Error setting up listener. Error: %s", err)
		return err
	}

	//signal that socket is being served
	servingCh <- struct{}{}

	if err := http.Serve(l, r); err != nil {
		logrus.Errorf("Error listening for http requests. Error: %s", err)
		return err
	}

	return nil
}

type postCallback func(req *APIRequest) error

func post(postCb postCallback) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// process data from request body, if any
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		req := APIRequest{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// process data from url, if any
		vars := mux.Vars(r)
		if vars["tag"] != "" {
			req.Nodes = append(req.Nodes, vars["tag"])
		}
		if vars["addr"] != "" {
			req.Addrs = append(req.Addrs, vars["addr"])
		}

		// process query variables
		req.ExtraVars, err = validateAndSanitizeEmptyExtraVars("extra_vars", req.ExtraVars)
		if err != nil {
			http.Error(w,
				err.Error(),
				http.StatusInternalServerError)
			return
		}

		// call the handler
		if err := postCb(&req); err != nil {
			http.Error(w,
				err.Error(),
				http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
}

func validateAndSanitizeEmptyExtraVars(errorPrefix, extraVars string) (string, error) {
	if strings.TrimSpace(extraVars) == "" {
		return configuration.DefaultValidJSON, nil
	}

	// extra vars string should be valid json.
	vars := &map[string]interface{}{}
	if err := json.Unmarshal([]byte(extraVars), vars); err != nil {
		logrus.Errorf("failed to parse json: '%s'. Error: %v", extraVars, err)
		return "", errInvalidJSON(errorPrefix, err)
	}
	return extraVars, nil
}

func (m *Manager) nodesCommission(req *APIRequest) error {
	me := newWaitableEvent(newCommissionEvent(m, req.Nodes, req.ExtraVars, req.HostGroup))
	m.reqQ <- me
	return me.waitForCompletion()
}

func (m *Manager) nodesDecommission(req *APIRequest) error {
	me := newWaitableEvent(newDecommissionEvent(m, req.Nodes, req.ExtraVars))
	m.reqQ <- me
	return me.waitForCompletion()
}

func (m *Manager) nodesUpdate(req *APIRequest) error {
	me := newWaitableEvent(newUpdateEvent(m, req.Nodes, req.ExtraVars, req.HostGroup))
	m.reqQ <- me
	return me.waitForCompletion()
}

func (m *Manager) nodesDiscover(req *APIRequest) error {
	me := newWaitableEvent(newDiscoverEvent(m, req.Addrs, req.ExtraVars))
	m.reqQ <- me
	return me.waitForCompletion()
}

func (m *Manager) globalsSet(req *APIRequest) error {
	me := newWaitableEvent(newSetGlobalsEvent(m, req.ExtraVars))
	m.reqQ <- me
	return me.waitForCompletion()
}

func (m *Manager) monitorEvent(req *APIRequest) error {
	var (
		e     event
		nodes []monitor.SubsysNode
	)

	for _, node := range req.Event.Nodes {
		nodes = append(nodes, monitor.NewNode(node.Label, node.Serial, node.MgmtAddr))
	}

	switch strings.ToLower(req.Event.Name) {
	case strings.ToLower(monitor.Discovered.String()):
		e = newDiscoveredEvent(m, nodes)
	case strings.ToLower(monitor.Disappeared.String()):
		e = newDisappearedEvent(m, nodes)
	default:
		return errInvalidEventName(req.Event.Name)
	}

	// XXX: revisit, do we need to process monitor events as waitable-events?
	m.reqQ <- e
	return nil
}

func (m *Manager) configSet(req *APIRequest) error {
	if req.Config == nil {
		return errNilConfig()
	}

	me := newWaitableEvent(newSetConfigEvent(m, req.Config))
	m.reqQ <- me
	return me.waitForCompletion()
}

type getCallback func(req *APIRequest) (io.Reader, error)

func get(getCb getCallback) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		req := &APIRequest{
			Nodes: []string{strings.TrimSpace(vars["tag"])},
			Job:   strings.TrimSpace(vars["job"]),
		}
		out, err := getCb(req)
		if err != nil {
			http.Error(w,
				err.Error(),
				http.StatusInternalServerError)
			return
		}
		// can't use a zero value of slice here as the byte Reader returned by
		// bytes package checks for 0 length slice and returns without error
		buf := make([]byte, 128)
		for {
			n, err := out.Read(buf)
			if n > 0 {
				if _, err := w.Write(buf[:n]); err != nil {
					logrus.Errorf("failed to write response bytes '%s'. Error: %v", buf, err)
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}
}

func (m *Manager) oneNode(req *APIRequest) (io.Reader, error) {
	node, err := m.findNode(req.Nodes[0])
	if err != nil {
		return nil, err
	}

	out, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(out), nil
}

func (m *Manager) allNodes(noop *APIRequest) (io.Reader, error) {
	out, err := json.Marshal(m.nodes)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(out), nil
}

func (m *Manager) globalsGet(noop *APIRequest) (io.Reader, error) {
	globals := m.configuration.GetGlobals()
	globalData := struct {
		ExtraVars map[string]interface{} `json:"extra_vars"`
	}{
		ExtraVars: make(map[string]interface{}),
	}
	if err := json.Unmarshal([]byte(globals), &globalData.ExtraVars); err != nil {
		return nil, err
	}
	out, err := json.Marshal(globalData)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(out), nil
}

func (m *Manager) jobGet(req *APIRequest) (io.Reader, error) {
	var j *Job
	switch req.Job {
	case jobLabelActive:
		j = m.activeJob
	case jobLabelLast:
		j = m.lastJob
	default:
		return nil, errInvalidJobLabel(req.Job)
	}

	if j == nil {
		return nil, errJobNotExist(req.Job)
	}

	out, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(out), nil
}

func (m *Manager) logsGet(req *APIRequest) (io.Reader, error) {
	var j *Job
	switch req.Job {
	case jobLabelActive:
		j = m.activeJob
	case jobLabelLast:
		j = m.lastJob
	default:
		return nil, errInvalidJobLabel(req.Job)
	}

	if j == nil {
		return nil, errJobNotExist(req.Job)
	}

	r, w := io.Pipe()
	if err := j.PipeLogs(w); err != nil {
		return nil, err
	}

	return r, nil
}

func (m *Manager) configGet(noop *APIRequest) (io.Reader, error) {
	out, err := json.Marshal(m.config)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(out), nil
}
