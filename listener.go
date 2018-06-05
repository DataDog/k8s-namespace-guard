// Copyright 2017 Yahoo Holdings Inc.
// Licensed under the terms of the 3-Clause BSD License.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/golang/glog"
	admissionv1 "k8s.io/api/admission/v1beta1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	bypassAnnotationKey = "k8s-namespace-guard.admission.yahoo.com/allow-cascade-delete"
)

var (
	namespaceResourceType = v1.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
)

// writeResponse writes the admissionReviewStatus object to the response body
func writeResponse(rw http.ResponseWriter, admReview *admissionv1.AdmissionReview, allowed bool, errorMsg string) {
	glog.Infof("Responding Allowed: %t for %s on Namespace: %s by user: %s", allowed,
		admReview.Request.Operation,
		admReview.Request.Name,
		admReview.Request.UserInfo.Username)

	if !allowed {
		glog.Errorf("Rejection reason: %s", errorMsg)
	}

	admReview.Response = &admissionv1.AdmissionResponse{
		Allowed: allowed,
		Result: &v1.Status{
			Reason: v1.StatusReason(errorMsg),
		},
	}

	body := new(bytes.Buffer)
	err := json.NewEncoder(body).Encode(admReview)
	if err != nil {
		io.WriteString(rw, "Error occurred while encoding the admission review status into json: "+err.Error())
		return
	}
	rw.Write(body.Bytes())
}

func podCounter(namespace string) (int, error) {
	list, err := clientset.CoreV1().Pods(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func serviceCounter(namespace string) (int, error) {
	list, err := clientset.CoreV1().Services(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func replicasetCounter(namespace string) (int, error) {
	list, err := clientset.ExtensionsV1beta1().ReplicaSets(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func deploymentCounter(namespace string) (int, error) {
	list, err := clientset.AppsV1beta1().Deployments(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func statefulsetCounter(namespace string) (int, error) {
	list, err := clientset.AppsV1beta1().StatefulSets(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func daemonsetCounter(namespace string) (int, error) {
	list, err := clientset.ExtensionsV1beta1().DaemonSets(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func ingressCounter(namespace string) (int, error) {
	list, err := clientset.ExtensionsV1beta1().Ingresses(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func autoScaleCounter(namespace string) (int, error) {
	list, err := clientset.AutoscalingV1().HorizontalPodAutoscalers(namespace).List(v1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

// validateNamespaceDeletion returns an error if the namespace contains any workload resources
func validateNamespaceDeletion(namespace string) (err error) {

	counters := []struct {
		kind    string
		counter func(namespace string) (int, error)
	}{
		{"pods", podCounter},
		{"services", serviceCounter},
		{"replicasets", replicasetCounter},
		{"deployments", deploymentCounter},
		{"statefulsets", statefulsetCounter},
		{"daemonsets", daemonsetCounter},
		{"ingresses", ingressCounter},
		{"horizontalpodautoscalers", autoScaleCounter},
	}

	var errList []error
	var nonEmptyList []string

	for _, c := range counters {
		num, err := c.counter(namespace)
		if err != nil {
			errList = append(errList, fmt.Errorf("error listing %s, %v", c.kind, err))
			continue
		}
		if num > 0 {
			nonEmptyList = append(nonEmptyList, fmt.Sprintf("%s(%d)", c.kind, num))
		}
	}

	errStr := ""
	if len(nonEmptyList) > 0 {
		errStr += fmt.Sprintf("The namespace %s you are trying to remove contains one or more of these resources: %v. Please delete them and try again.", namespace, nonEmptyList)
	}
	if len(errList) > 0 {
		errStr += fmt.Sprintf("The following error(s) occurred while validating the DELETE operation on the namespace %s: %v.", namespace, errList)
	}
	if errStr != "" {
		errStr += fmt.Sprintf(" WARNING: If you know what you are doing, run `kubectl annotate namespace %s %s=true` to bypass this policy check.", namespace, bypassAnnotationKey)
		return errors.New(errStr)
	}
	return nil
}

// webhookHandler handles the namespace deletion guard admission webhook
func webhookHandler(rw http.ResponseWriter, req *http.Request) {
	glog.Infof("Serving %s %s request for client: %s", req.Method, req.URL.Path, req.RemoteAddr)

	if req.Method != http.MethodPost {
		http.Error(rw, fmt.Sprintf("Incoming request method %s is not supported, only POST is supported", req.Method), http.StatusMethodNotAllowed)
		return
	}

	if req.URL.Path != "/" {
		http.Error(rw, fmt.Sprintf("%s 404 Not Found", req.URL.Path), http.StatusNotFound)
		return
	}

	admReview := admissionv1.AdmissionReview{}
	err := json.NewDecoder(req.Body).Decode(&admReview)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to decode the request body json into an AdmissionReview resource: %s", err.Error())
		writeResponse(rw, &admissionv1.AdmissionReview{
			Request: new(admissionv1.AdmissionRequest),
		}, false, errorMsg)
		return
	}
	glog.Infof("Incoming AdmissionReview for %s on resource: %v, kind: %v", admReview.Request.Operation, admReview.Request.Resource, admReview.Request.Kind)

	if *admitAll == true {
		glog.Warningf("admitAll flag is set to true. Allowing Namespace admission review request to pass without validation.")
		writeResponse(rw, &admReview, true, "")
		return
	}

	if admReview.Request.Resource != namespaceResourceType {
		errorMsg := fmt.Sprintf("Incoming resource is not a Namespace: %v", admReview.Request.Resource)
		writeResponse(rw, &admReview, false, errorMsg)
		return
	}

	if admReview.Request.Operation != admissionv1.Delete {
		errorMsg := fmt.Sprintf("Incoming operation is %v on namespace %s. Only DELETE is currently supported.", admReview.Request.Operation, admReview.Request.Name)
		writeResponse(rw, &admReview, false, errorMsg)
		return
	}

	namespace, err := clientset.CoreV1().Namespaces().Get(admReview.Request.Name, v1.GetOptions{})
	if err != nil {
		// If the namespace is not found, approve the request and let apiserver handle the case
		// For any other error, reject the request
		if apiErrors.IsNotFound(err) {
			glog.Infof("Namespace %s not found, let apiserver handle the error: %s", admReview.Request.Name, err.Error())
			writeResponse(rw, &admReview, true, "")
		} else {
			errorMsg := fmt.Sprintf("Error occurred while retrieving the namespace %s: %s", admReview.Request.Name, err.Error())
			writeResponse(rw, &admReview, false, errorMsg)
		}
		return
	}

	if annotations := namespace.GetAnnotations(); annotations != nil {
		if annotations[bypassAnnotationKey] == "true" {
			glog.Infof("Namespace %s has the bypass annotation set[%s:true]. OK to DELETE.", admReview.Request.Name, bypassAnnotationKey)
			writeResponse(rw, &admReview, true, "")
			return
		}
	}

	err = validateNamespaceDeletion(admReview.Request.Name)
	if err != nil {
		writeResponse(rw, &admReview, false, err.Error())
		return
	}

	glog.Infof("Namespace %s does not contain any workload resources. OK to DELETE.", admReview.Request.Name)
	writeResponse(rw, &admReview, true, "")
}
