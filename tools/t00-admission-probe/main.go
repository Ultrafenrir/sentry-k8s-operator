package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"reflect"
)

type admissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *admissionRequest  `json:"request,omitempty"`
	Response   *admissionResponse `json:"response,omitempty"`
}

type admissionRequest struct {
	UID       string          `json:"uid"`
	Operation string          `json:"operation"`
	Object    json.RawMessage `json:"object"`
	OldObject json.RawMessage `json:"oldObject"`
}

type admissionResponse struct {
	UID       string  `json:"uid"`
	Allowed   bool    `json:"allowed"`
	Status    *status `json:"status,omitempty"`
	Patch     []byte  `json:"patch,omitempty"`
	PatchType *string `json:"patchType,omitempty"`
}

type status struct {
	Message string `json:"message"`
}

type probeObject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Project             map[string]any `json:"project"`
		NotificationTargets map[string]any `json:"notificationTargets,omitempty"`
		DSN                 map[string]any `json:"dsn,omitempty"`
	} `json:"spec"`
}

func main() {
	address := flag.String("address", ":8443", "HTTPS listen address")
	certFile := flag.String("cert-file", "/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("key-file", "/tls/tls.key", "TLS private key")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/mutate", admissionHandler(mutate))
	mux.HandleFunc("/validate", admissionHandler(validate))
	server := &http.Server{Addr: *address, Handler: mux, ReadHeaderTimeout: 5_000_000_000}
	log.Fatal(server.ListenAndServeTLS(*certFile, *keyFile))
}

func admissionHandler(evaluate func(admissionRequest) admissionResponse) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var review admissionReview
		if err := json.NewDecoder(request.Body).Decode(&review); err != nil || review.Request == nil {
			http.Error(writer, "invalid AdmissionReview", http.StatusBadRequest)
			return
		}
		response := evaluate(*review.Request)
		response.UID = review.Request.UID
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(admissionReview{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
			Response:   &response,
		}); err != nil {
			log.Printf("encode AdmissionReview: %v", err)
		}
	}
}

func mutate(request admissionRequest) admissionResponse {
	var object probeObject
	if err := json.Unmarshal(request.Object, &object); err != nil {
		return denied(fmt.Sprintf("decode object: %v", err))
	}
	if _, exists := object.Spec.DSN["secret"]; exists {
		return admissionResponse{Allowed: true}
	}
	path := "/spec/dsn"
	value := map[string]any{"secret": object.Metadata.Name + "-sentry"}
	if object.Spec.DSN != nil {
		path = "/spec/dsn/secret"
		value = nil
	}
	var operations []map[string]any
	if value == nil {
		operations = []map[string]any{{"op": "add", "path": path, "value": object.Metadata.Name + "-sentry"}}
	} else {
		operations = []map[string]any{{"op": "add", "path": path, "value": value}}
	}
	patch, err := json.Marshal(operations)
	if err != nil {
		return denied(fmt.Sprintf("encode patch: %v", err))
	}
	patchType := "JSONPatch"
	return admissionResponse{Allowed: true, Patch: patch, PatchType: &patchType}
}

func validate(request admissionRequest) admissionResponse {
	if request.Operation != "UPDATE" {
		return admissionResponse{Allowed: true}
	}
	var oldObject, newObject probeObject
	if err := json.Unmarshal(request.OldObject, &oldObject); err != nil {
		return denied(fmt.Sprintf("decode old object: %v", err))
	}
	if err := json.Unmarshal(request.Object, &newObject); err != nil {
		return denied(fmt.Sprintf("decode new object: %v", err))
	}
	if !reflect.DeepEqual(oldObject.Spec.Project, newObject.Spec.Project) {
		return denied("spec.project is immutable")
	}
	return admissionResponse{Allowed: true}
}

func denied(message string) admissionResponse {
	return admissionResponse{Allowed: false, Status: &status{Message: message}}
}
