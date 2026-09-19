package main

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The model list refresh is a GET: it sends no body. The COSY signature covers
// the request body, so signing an encoded "{}" while sending nothing made the
// gateway answer 403 "Signature invalid" for every refresh — which the plugin
// silently swallowed by falling back to its static list, so newly published
// upstream models never reached clients.
//
// These tests pin the invariant that keeps that from coming back: the bytes
// covered by the signature are the bytes actually sent.
func stubModelListServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func testStoredAuth() *storedAuth {
	// newCosySession derives everything but the uid from random values, so a
	// token-shaped string is enough: it is never dereferenced, only hashed.
	sa := &storedAuth{}
	sa.Auth.AccessToken = "dt-test-header.dt-test-payload.dt-test-signature"
	sa.Account.UID = "uid-signature-test"
	return sa
}

// modelListRequestURL is the stub equivalent of endpointModels, with the same
// query string the real endpoint carries.
func modelListRequestURL(server *httptest.Server) string {
	return server.URL + "/algo/api/v2/model/list?Encode=1"
}

func TestCallModelsAPISigIsComputedOverTheSentBody(t *testing.T) {
	var sentBody []byte
	var gotAuth, gotDate, gotPath string

	server := stubModelListServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			sentBody, _ = io.ReadAll(r.Body)
		}
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("cosy-date")
		gotPath = strings.TrimPrefix(r.URL.Path, "/algo")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":[{"key":"fresh-model","display_name":"Fresh","enable":true,"max_input_tokens":180000}]}`))
	})

	sa := testStoredAuth()
	sess, err := cosySessionFor(sa)
	if err != nil {
		t.Fatalf("cosy session: %v", err)
	}

	models, err := fetchModelListFrom(sa, modelListRequestURL(server))
	if err != nil {
		t.Fatalf("fetchModelListFrom: %v", err)
	}
	if len(models) != 1 || models[0].ID != "fresh-model" {
		t.Fatalf("models = %#v", models)
	}

	// The server verifies over what it received. Recompute it the same way.
	if len(sentBody) != 0 {
		t.Fatalf("model list request carried a body %q; a signed GET must send nothing", sentBody)
	}
	parts := strings.Split(gotAuth, ".")
	if len(parts) != 3 || parts[0] != "Bearer COSY" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("authorization shape = %q", gotAuth)
	}
	sum := md5.Sum([]byte(parts[1] + "\n" + sess.CosyKey + "\n" + gotDate + "\n" + string(sentBody) + "\n" + gotPath))
	if want := hex.EncodeToString(sum[:]); want != parts[2] {
		t.Fatalf("signature over the sent body = %s, but the header carries %s", want, parts[2])
	}

	// Guard the regression directly: signing the encoded "{}" body again would
	// reproduce exactly the mismatch the gateway used to reject.
	stale := md5.Sum([]byte(parts[1] + "\n" + sess.CosyKey + "\n" + gotDate + "\n" + string(qoderEncode([]byte("{}"))) + "\n" + gotPath))
	if hex.EncodeToString(stale[:]) == parts[2] {
		t.Fatal("signature is still computed over an encoded body that is never sent")
	}
}

func TestCallModelsAPIFailsWhenSignatureIsRejected(t *testing.T) {
	server := stubModelListServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"101","message":"Signature invalid"}`))
	})

	if _, err := fetchModelListFrom(testStoredAuth(), modelListRequestURL(server)); err == nil {
		t.Fatal("a rejected signature was reported as success")
	}
}

func TestCallModelsAPIParsesCatalogAndDropsDisabled(t *testing.T) {
	server := stubModelListServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"chat":[` +
			`{"key":"enabled-model","display_name":"Enabled","enable":true,"max_input_tokens":200000},` +
			`{"key":"disabled-model","display_name":"Disabled","enable":false,"max_input_tokens":200000}]}`))
	})

	models, err := fetchModelListFrom(testStoredAuth(), modelListRequestURL(server))
	if err != nil {
		t.Fatalf("fetchModelListFrom: %v", err)
	}
	if len(models) != 1 || models[0].ID != "enabled-model" {
		t.Fatalf("models = %#v", models)
	}
	if models[0].ContextLength != 200000 {
		t.Fatalf("context length = %d, want the upstream value", models[0].ContextLength)
	}
}
