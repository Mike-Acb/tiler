package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Kube 是只覆盖控制台所需接口的极简 k8s REST 客户端。
// 集群内用 ServiceAccount 凭证；本地调试设置 KUBE_API（如 kubectl proxy 的 http://127.0.0.1:8001）。
type Kube struct {
	base   string
	token  string
	ns     string
	dryRun bool
	http   *http.Client // 普通请求，带超时
	stream *http.Client // 日志流，不设整体超时
}

var errConflict = errors.New("资源已被修改，请重试")
var errNotFound = errors.New("资源不存在")

func newKube(ns string, dryRun bool) (*Kube, error) {
	k := &Kube{ns: ns, dryRun: dryRun}
	tr := &http.Transport{TLSClientConfig: &tls.Config{}}
	if api := os.Getenv("KUBE_API"); api != "" {
		k.base = strings.TrimRight(api, "/")
		k.token = os.Getenv("KUBE_TOKEN")
	} else {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, fmt.Errorf("不在集群内，且未设置 KUBE_API")
		}
		k.base = "https://" + host + ":" + port
		tok, err := os.ReadFile(saDir + "/token")
		if err != nil {
			return nil, fmt.Errorf("读取 ServiceAccount token 失败: %w", err)
		}
		k.token = strings.TrimSpace(string(tok))
		ca, err := os.ReadFile(saDir + "/ca.crt")
		if err != nil {
			return nil, fmt.Errorf("读取 ServiceAccount CA 失败: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		tr.TLSClientConfig.RootCAs = pool
	}
	k.http = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	k.stream = &http.Client{Transport: tr}
	return k, nil
}

func (k *Kube) request(client *http.Client, method, path string, query url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := k.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return client.Do(req)
}

func (k *Kube) do(method, path string, query url.Values, body, out any) error {
	if query == nil {
		query = url.Values{}
	}
	if k.dryRun && method != http.MethodGet {
		query.Set("dryRun", "All")
	}
	resp, err := k.request(k.http, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusConflict:
		return errConflict
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode >= 300:
		var st struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &st) == nil && st.Message != "" {
			return fmt.Errorf("k8s %s %s: %d %s", method, path, resp.StatusCode, st.Message)
		}
		return fmt.Errorf("k8s %s %s: %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---- ConfigMap ----

type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	CreationTimestamp *time.Time        `json:"creationTimestamp,omitempty"`
}

type ConfigMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Data       map[string]string `json:"data"`
}

func (k *Kube) GetConfigMap(name string) (*ConfigMap, error) {
	var cm ConfigMap
	err := k.do(http.MethodGet, "/api/v1/namespaces/"+k.ns+"/configmaps/"+url.PathEscape(name), nil, nil, &cm)
	if err != nil {
		return nil, err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	return &cm, nil
}

// UpdateConfigMap 用 resourceVersion 做乐观锁，别人改过会返回 errConflict。
func (k *Kube) UpdateConfigMap(cm *ConfigMap) error {
	cm.APIVersion, cm.Kind = "v1", "ConfigMap"
	return k.do(http.MethodPut, "/api/v1/namespaces/"+k.ns+"/configmaps/"+url.PathEscape(cm.Metadata.Name), nil, cm, nil)
}

// ---- Job / Pod ----

type Job struct {
	Metadata ObjectMeta     `json:"metadata"`
	Spec     map[string]any `json:"spec"`
	Status   struct {
		Active         int        `json:"active"`
		Succeeded      int        `json:"succeeded"`
		Failed         int        `json:"failed"`
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
		Conditions     []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// Phase 归纳 Job 状态：Running / Succeeded / Failed / Pending。
func (j *Job) Phase() string {
	for _, c := range j.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete", "SuccessCriteriaMet":
			return "Succeeded"
		case "Failed", "FailureTarget":
			return "Failed"
		}
	}
	if j.Status.Active > 0 {
		return "Running"
	}
	return "Pending"
}

func (k *Kube) ListJobs(selector string) ([]Job, error) {
	var list struct {
		Items []Job `json:"items"`
	}
	q := url.Values{}
	if selector != "" {
		q.Set("labelSelector", selector)
	}
	err := k.do(http.MethodGet, "/apis/batch/v1/namespaces/"+k.ns+"/jobs", q, nil, &list)
	return list.Items, err
}

func (k *Kube) GetJob(name string) (*Job, error) {
	var j Job
	err := k.do(http.MethodGet, "/apis/batch/v1/namespaces/"+k.ns+"/jobs/"+url.PathEscape(name), nil, nil, &j)
	return &j, err
}

func (k *Kube) CreateJob(job map[string]any) (*Job, error) {
	var j Job
	err := k.do(http.MethodPost, "/apis/batch/v1/namespaces/"+k.ns+"/jobs", nil, job, &j)
	return &j, err
}

func (k *Kube) DeleteJob(name string) error {
	body := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": "Background"}
	return k.do(http.MethodDelete, "/apis/batch/v1/namespaces/"+k.ns+"/jobs/"+url.PathEscape(name), nil, body, nil)
}

type ContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Running *struct {
			StartedAt *time.Time `json:"startedAt"`
		} `json:"running"`
		Terminated *struct {
			ExitCode   int        `json:"exitCode"`
			Reason     string     `json:"reason"`
			FinishedAt *time.Time `json:"finishedAt"`
		} `json:"terminated"`
	} `json:"state"`
}

type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		NodeName       string `json:"nodeName"`
		InitContainers []struct {
			Name string `json:"name"`
		} `json:"initContainers"`
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []ContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

func (k *Kube) ListPods(selector string) ([]Pod, error) {
	var list struct {
		Items []Pod `json:"items"`
	}
	q := url.Values{"labelSelector": {selector}}
	err := k.do(http.MethodGet, "/api/v1/namespaces/"+k.ns+"/pods", q, nil, &list)
	return list.Items, err
}

// StreamLogs 返回容器日志流，调用方负责关闭。
func (k *Kube) StreamLogs(pod, container string, tail int, follow bool) (io.ReadCloser, error) {
	q := url.Values{"container": {container}, "tailLines": {fmt.Sprint(tail)}}
	if follow {
		q.Set("follow", "true")
	}
	resp, err := k.request(k.stream, http.MethodGet, "/api/v1/namespaces/"+k.ns+"/pods/"+url.PathEscape(pod)+"/log", q, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		var st struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &st) == nil && st.Message != "" {
			return nil, fmt.Errorf("%s", st.Message)
		}
		return nil, fmt.Errorf("读取日志失败: %d", resp.StatusCode)
	}
	return resp.Body, nil
}
