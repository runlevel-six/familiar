// SPDX-License-Identifier: Apache-2.0

package prom

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueryDropsNonFinite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"m":"a"},"value":[1,"0.5"]},{"metric":{"m":"b"},"value":[1,"NaN"]},{"metric":{"m":"c"},"value":[1,"+Inf"]}]}}`)
	}))
	defer srv.Close()
	got, err := New(srv.URL).Query(context.Background(), "x")
	if err != nil || len(got) != 1 || got[0].Labels["m"] != "a" || got[0].Value != 0.5 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestQueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Query(context.Background(), "x"); err == nil {
		t.Fatal("error not returned")
	}
}
