/*This file is part of kuberpult.

Kuberpult is free software: you can redistribute it and/or modify
it under the terms of the Expat(MIT) License as published by
the Free Software Foundation.

Kuberpult is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
MIT License for more details.

You should have received a copy of the MIT License
along with kuberpult. If not, see <https://directory.fsf.org/wiki/License:Expat>.

Copyright 2023 freiheit.com*/

package auth

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MicahParks/keyfunc/v2"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/go-cmp/cmp"
)

func TestValidateTokenStatic(t *testing.T) {
	tcs := []struct {
		Name          string
		Token         string
		ExpectedError string
		noInit        bool
	}{
		{
			Name:          "Not a token",
			Token:         "asdf",
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
		},
		{
			Name:          "Not initialized",
			Token:         "asdf",
			noInit:        true,
			ExpectedError: "JWKS not initialized.",
		},
		{
			Name:          "Not a token 2",
			Token:         "asdf.asdf.asdf",
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: could not JSON decode header: invalid character 'j' looking for beginning of value",
		},
		{
			Name:          "Kid not present",
			Token:         "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.WDlNbJFe8ZX6C1mS27xwxg-9tk8vtkk6sDgucRj8xW0",
			ExpectedError: "Failed to parse the JWT.\nError: token is unverifiable: error while executing keyfunc: the JWT has an invalid kid: could not find kid in JWT header",
		},
		{
			Name:          "Kid not part of jwks",
			Token:         "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCIsImtpZCI6ImFzZGYifQ.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.aNyAK8qpCScGchUmv1q1pBXOddWKN8_7agLUo7pXDog",
			ExpectedError: "Failed to parse the JWT.\nError: token is unverifiable: error while executing keyfunc: the given key ID was not found in the JWKS",
		},
	}

	var jwks, err = JWKSInitAzureFromJson()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if tc.noInit {
				_, err = ValidateToken(tc.Token, nil, "clientId", "tenantId")
			} else {
				_, err = ValidateToken(tc.Token, jwks, "clientId", "tenantId")
			}
			if diff := cmp.Diff(err.Error(), tc.ExpectedError); diff != "" {
				t.Errorf("Error mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func getToken(clientId string, tenantId string, kid string, expiry int64, name string, email string) (string, error) {
	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(`-----BEGIN RSA PRIVATE KEY-----
MIICXQIBAAKBgQC/oyqURHIPNzx4vcKrUUZYr6Bxq2OSD44a63zeIDA1oZkR+sac
tmkub+8NI49GqrbssWf944v3ZLp8KXMh6i+U9pkSdDfvKcQUProQ+Tlm/m0SFXa6
h7vq6iVD1uawzN9aQaR7WiKV1TuPGUgE86/l+XTvLZ/MbKh0tz9j8JtY4QIDAQAB
AoGBAICNeROq8oSIfjVUvlDkHXeCoPN/kDS74IzoaYQsPYrMk30/J5qatuYiyk6b
CxLRlBIlU+g5i3vygzKlL4mRqkZuCM4xPbpuW9sdZp61TxWZk7Tm+SYBTStYSGkT
tPmvnKsYWkUh1WDSkeLJqHkRbQXAZJkAKRMYgLu2F29fWOZBAkEA8P31nm/AiDiD
dkGSGp4GVQ5BBry3XdP3c6rfzmW8sMElxqoj2watdia72+grf8eVo8vtsTiOrVUD
ZoS5C5GKKQJBAMuSXXQZrBa4qB7YkGi5ysQRQZoegdYZa44q9L9oBE/iEl/ejR1l
EKZi+v2greoIruqczGAD7VbEiwT50+npH/kCQQDJgpGvOaK0RQ0oBQw2VYzV8mVN
TN/HBUcU4PzjiQ6OffMoe3wf2SWSdjD/YNN+tVTa8dp/Jdun9D4zqydQFRKBAkBV
zlPl5AxNZ3g1yELWYbm9+ygTtlgzznMvcZvIMiffJANqtXv1r+vctkvlLB0iUJap
/X2H2x/nOuD+L+/K4KDBAkAHcO3Gv7VZsSHfnd/JfDzxtL0MFWerGZyGlaNFmX27
1dWRXvcS5A0zPMgiBWfvHFx2DpSiceffqnis+UryeE+L
-----END RSA PRIVATE KEY-----`))
	claims := jwt.MapClaims{}
	if len(clientId) > 0 {
		claims["aud"] = clientId
	}
	if len(tenantId) > 0 {
		claims["tid"] = tenantId
	}
	if len(name) > 0 {
		claims["name"] = name
	}
	if len(email) > 0 {
		claims["email"] = email
	}

	claims["exp"] = expiry
	jwtToken := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	jwtToken.Header["kid"] = kid
	tokenString, err := jwtToken.SignedString(privateKey)
	if err != nil {
		return "", fmt.Errorf("Could not sign token %s", err.Error())
	}
	return tokenString, nil
}

func getJwks() (*keyfunc.JWKS, error) {
	publicKey, err := jwt.ParseRSAPublicKeyFromPEM([]byte(`-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQC/oyqURHIPNzx4vcKrUUZYr6Bx
q2OSD44a63zeIDA1oZkR+sactmkub+8NI49GqrbssWf944v3ZLp8KXMh6i+U9pkS
dDfvKcQUProQ+Tlm/m0SFXa6h7vq6iVD1uawzN9aQaR7WiKV1TuPGUgE86/l+XTv
LZ/MbKh0tz9j8JtY4QIDAQAB
-----END PUBLIC KEY-----`))
	if err != nil {
		return nil, err
	}
	givenKey := keyfunc.NewGivenRSA(publicKey, keyfunc.GivenKeyOptions{})
	keys := map[string]keyfunc.GivenKey{
		"testKey": givenKey,
	}
	return keyfunc.NewGiven(keys), nil
}

func TestValidateTokenGenerated(t *testing.T) {
	tcs := []struct {
		Name          string
		ClientId      string
		TenantId      string
		ExpectedError string
		Expiry        int64
		Kid           string
	}{
		{
			Name:          "invalid client id",
			ClientId:      "invalidClient",
			TenantId:      "tenantId",
			ExpectedError: "Unknown client id provided: invalidClient",
			Kid:           "testKey",
		},
		{
			Name:          "No client id",
			ClientId:      "",
			TenantId:      "tenantId",
			ExpectedError: "Client id not found in token.",
			Kid:           "testKey",
		},
		{
			Name:          "invalid tenant id",
			ClientId:      "clientId",
			TenantId:      "invalidTenant",
			ExpectedError: "Unknown tenant id provided: invalidTenant",
			Kid:           "testKey",
		},
		{
			Name:          "No tenant id",
			ClientId:      "clientId",
			TenantId:      "",
			ExpectedError: "Tenant id not found in token.",
			Kid:           "testKey",
		},
		{
			Name:          "invalid  kid",
			ClientId:      "clientId",
			TenantId:      "tenantId",
			ExpectedError: "Failed to parse the JWT.\nError: token is unverifiable: error while executing keyfunc: the given key ID was not found in the JWKS",
			Kid:           "tests",
		},
		{
			Name:          "Expired key",
			ClientId:      "clientId",
			TenantId:      "tenantId",
			ExpectedError: "Failed to parse the JWT.\nError: token has invalid claims: token is expired",
			Expiry:        time.Now().Unix(),
			Kid:           "testKey",
		},
		{
			Name:     "valid key",
			ClientId: "clientId",
			TenantId: "tenantId",
			Kid:      "testKey",
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			duration, err := time.ParseDuration("10m")
			if err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(duration).Unix()
			if tc.Expiry != 0 {
				expiry = tc.Expiry
			}
			tokenString, err := getToken(tc.ClientId, tc.TenantId, tc.Kid, expiry, "", "")
			if err != nil {
				t.Fatal(err)
			}
			jwks, err := getJwks()
			if err != nil {
				t.Fatal(err)
			}
			_, err = ValidateToken(tokenString, jwks, "clientId", "tenantId")
			if len(tc.ExpectedError) > 0 {
				if err == nil {
					t.Fatalf("Expected error \n%s, got nil", tc.ExpectedError)
				}
				if diff := cmp.Diff(err.Error(), tc.ExpectedError); diff != "" {
					t.Fatalf("Error mismatch (-want +got):\n%s", diff)
				}
			} else {
				if err != nil {
					t.Fatalf("Expected no error got):\n%s", err.Error())
				}
			}
		})
	}
}

func TestHttpMiddleware(t *testing.T) {
	tcs := []struct {
		Name          string
		Path          string
		Method        string
		ExpectedError string
		Authenticated bool
	}{
		{
			Name:          "root path",
			Path:          "/",
			Method:        http.MethodGet,
			ExpectedError: "",
		},
		{
			Name:          "js path",
			Path:          "/static/js/content.js",
			Method:        http.MethodGet,
			ExpectedError: "",
		},
		{
			Name:          "css path",
			Path:          "/static/css/content.css",
			Method:        http.MethodGet,
			ExpectedError: "",
		},
		{
			Name:          "api call - wrong url",
			Path:          "/environment/production/locks/999",
			Method:        http.MethodGet,
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
			Authenticated: false,
		},
		{
			Name:          "api call - wrong url path",
			Path:          "/environment/production/releasetrainisawsome",
			Method:        http.MethodGet,
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
			Authenticated: false,
		},
		{
			Name:          "api call rleasetrain",
			Path:          "/environments/production/releasetrain",
			Method:        http.MethodGet,
			ExpectedError: "",
			Authenticated: false,
		},
		{
			Name:          "api call ",
			Path:          "/environments/production/locks/999",
			Method:        http.MethodGet,
			ExpectedError: "",
			Authenticated: false,
		},
		{
			Name:          "api call create environment POST",
			Path:          "/environments/dev",
			Method:        http.MethodPost,
			ExpectedError: "",
			Authenticated: false,
		},
		{
			Name:          "api call create environment GET",
			Path:          "/environments/dev",
			Method:        http.MethodGet,
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
			Authenticated: false,
		},
		{
			Name:          "api call create environment wrong url",
			Path:          "/environments/dev/something",
			Method:        http.MethodPost,
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
			Authenticated: false,
		},
		{
			Name:          "api call create environment another wrong url GET",
			Path:          "/environments/something/dev",
			Method:        http.MethodPost,
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
			Authenticated: false,
		},
		{
			Name:          "api call create environment another wrong url POST",
			Path:          "/environments/something/dev",
			Method:        http.MethodPost,
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
			Authenticated: false,
		},
		{
			Name:          "api call create environment - no env",
			Path:          "/environments/",
			Method:        http.MethodPost,
			ExpectedError: "Failed to parse the JWT.\nError: token is malformed: token contains an invalid number of segments",
			Authenticated: false,
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			r := strings.NewReader("Test message incoming")
			sr := io.Reader(r)
			req, err := http.NewRequest(tc.Method, tc.Path, sr)
			if err != nil {
				t.Fatal(err)
			}
			duration, err := time.ParseDuration("10m")
			if err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(duration).Unix()
			tokenString, err := getToken("clientId", "tenantId", "testKey", expiry, "testName", "test.email@com")
			if err != nil {
				t.Fatal(err)
			}
			jwks, err := getJwks()
			if err != nil {
				t.Fatal(err)
			}

			if tc.Authenticated {
				req.Header.Set("Authorization", tokenString)
			}
			testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				err := HttpAuthMiddleWare(w, r, jwks, "clientId", "tenantId", []string{"/"}, []string{"/static/js", "/static/css"})
				if len(tc.ExpectedError) > 0 {

					if err == nil {
						t.Fatalf("Expected error %s, got nil", tc.ExpectedError)
					}
					if diff := cmp.Diff(err.Error(), tc.ExpectedError); diff != "" {
						t.Fatalf("Error mismatch (-want +got):\n%s", diff)
					}
				} else {
					if tc.Authenticated {
						username := req.Header.Get("username")
						email := req.Header.Get("email")
						if username != "testName" {
							t.Fatalf("Expected username testName but got %q", username)
						}
						if email != "test.email@com" {
							t.Fatalf("Expected email test.email@com but got %q", email)
						}

					}
					if err != nil {
						t.Fatalf("Expected no error, got %s", err.Error())
					}
				}
			})
			rw := httptest.NewRecorder()
			handler := testHandler
			handler.ServeHTTP(rw, req)
		})
	}
}
