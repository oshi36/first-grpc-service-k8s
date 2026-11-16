package main

// Minimal Kubernetes REST client (no client-go: keeps the image tiny and the
// code readable on a slide). Uses the pod's ServiceAccount token in-cluster.

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

type Kube struct {
	base   string
	token  string
	client *http.Client
}

func NewKube() *Kube {
	k := &Kube{base: os.Getenv("KUBE_API"), client: &http.Client{Timeout: 5 * time.Second}}
	if k.base != "" { // local testing against a mock API
		return k
	}
	k.base = "https://kubernetes.default.svc"
	if b, err := os.ReadFile(saDir + "/token"); err == nil {
		k.token = strings.TrimSpace(string(b))
	}
	pool := x509.NewCertPool()
	if ca, err := os.ReadFile(saDir + "/ca.crt"); err == nil {
		pool.AppendCertsFromPEM(ca)
	}
	k.client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return k
}

func (k *Kube) do(method, path, ctype string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.base+path, rd)
	if err != nil {
		return err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ---- types (only the fields we use) ----------------------------------------

type probe struct {
	HTTPGet   *struct{} `json:"httpGet"`
	GRPC      *struct{} `json:"grpc"`
	TCPSocket *struct{} `json:"tcpSocket"`
	Exec      *struct{} `json:"exec"`
}

func (p *probe) kind() string {
	switch {
	case p == nil:
		return "none"
	case p.GRPC != nil:
		return "grpc"
	case p.HTTPGet != nil:
		return "httpGet"
	case p.TCPSocket != nil:
		return "tcpSocket"
	case p.Exec != nil:
		return "exec"
	}
	return "none"
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name              string  `json:"name"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
			CreationTimestamp string  `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			PodIP      string `json:"podIP"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Ready        bool `json:"ready"`
				RestartCount int  `json:"restartCount"`
				State        struct {
					Waiting    *struct{ Reason string } `json:"waiting"`
					Running    *struct{}                `json:"running"`
					Terminated *struct{ Reason string } `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type deployment struct {
	Spec struct {
		Replicas int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
					ReadinessProbe *probe `json:"readinessProbe"`
					LivenessProbe  *probe `json:"livenessProbe"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

type eventList struct {
	Items []struct {
		Reason         string `json:"reason"`
		Message        string `json:"message"`
		Count          int    `json:"count"`
		LastTimestamp  string `json:"lastTimestamp"`
		EventTime      string `json:"eventTime"`
		InvolvedObject struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"involvedObject"`
		Series *struct {
			LastObservedTime string `json:"lastObservedTime"`
		} `json:"series"`
	} `json:"items"`
}

type svcList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
}

// ---- calls -----------------------------------------------------------------

func (k *Kube) Pods(ns, selector string) (*podList, error) {
	var out podList
	err := k.do("GET", fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", ns, url.QueryEscape(selector)), "", nil, &out)
	return &out, err
}

func (k *Kube) Deployment(ns, name string) (*deployment, error) {
	var out deployment
	err := k.do("GET", fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name), "", nil, &out)
	return &out, err
}

func (k *Kube) WarningEvents(ns string) (*eventList, error) {
	var out eventList
	err := k.do("GET", fmt.Sprintf("/api/v1/namespaces/%s/events?fieldSelector=%s", ns, url.QueryEscape("type=Warning")), "", nil, &out)
	return &out, err
}

func (k *Kube) Services(ns, selector string) (*svcList, error) {
	var out svcList
	err := k.do("GET", fmt.Sprintf("/api/v1/namespaces/%s/services?labelSelector=%s", ns, url.QueryEscape(selector)), "", nil, &out)
	return &out, err
}

func (k *Kube) Scale(ns, name string, replicas int) error {
	return k.do("PATCH", fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s/scale", ns, name),
		"application/merge-patch+json", map[string]any{"spec": map[string]any{"replicas": replicas}}, nil)
}

func (k *Kube) DeletePod(ns, name string) error {
	return k.do("DELETE", fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, name), "", nil, nil)
}

// SetEnv sets (value != "") or removes (value == "") an env var on the
// greeter container. Changing the pod template triggers a rolling update.
func (k *Kube) SetEnv(ns, deploy, container, name, value string) error {
	env := map[string]any{"name": name, "value": value}
	if value == "" {
		env = map[string]any{"name": name, "$patch": "delete"}
	}
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": container, "env": []any{env}}},
	}}}}
	return k.do("PATCH", fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, deploy),
		"application/strategic-merge-patch+json", patch, nil)
}
