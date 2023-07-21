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
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"net/http"

	"github.com/freiheit-com/kuberpult/pkg/logger"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/httperrors"
	"google.golang.org/grpc/metadata"
)

type ctxMarker struct{}

var (
	ctxMarkerKey = &ctxMarker{}
)

/*
The frontend-service now defines the default author for git commits.
The frontend-service also allows overwriting the default values, see function `getRequestAuthorFromGoogleIAP`.
The cd-service generally expects these headers, either in the grpc context or the http headers.
*/
const (
	HeaderUserName  = "author-name"
	HeaderUserEmail = "author-email"
	HeaderUserRole  = "author-role"
)

func Encode64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func Decode64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}

// ReadUserFromContext returns a user from the ctx or an error if none was found, or it is invalid
func ReadUserFromContext(ctx context.Context) (*User, error) {
	u, ok := ctx.Value(ctxMarkerKey).(*User)
	if !ok || u == nil {
		return nil, httperrors.InternalError(ctx, errors.New("could not read user from context"))
	}
	return u, nil
}

// WriteUserToContext should be used in both frontend-service and cd-service.
// WriteUserToContext adds the User to the context for extraction later.
// The user must not be nil.
// Returning the new context that has been created.
func WriteUserToContext(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxMarkerKey, &u)
}

func WriteUserToGrpcContext(ctx context.Context, u User) context.Context {
	return metadata.AppendToOutgoingContext(ctx, HeaderUserEmail, Encode64(u.Email), HeaderUserName, Encode64(u.Name))
}

// WriteUserRoleToGrpcContext adds the user role to the GRPC context.
// Only used when RBAC is enabled.
func WriteUserRoleToGrpcContext(ctx context.Context, userRole string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, HeaderUserRole, Encode64(userRole))
}

// ReadUserFromGrpcContext should only be used in the cd-service.
// ReadUserFromGrpcContext takes the User from middleware (context).
// It returns a User or an error if the user is not found.
func ReadUserFromGrpcContext(ctx context.Context, dexEnabled bool) (*User, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, httperrors.AuthError(ctx, errors.New("could not retrieve metadata context with git author in grpc context"))
	}
	originalEmailArr := md.Get(HeaderUserEmail)
	if len(originalEmailArr) == 0 {
		return nil, httperrors.AuthError(ctx, errors.New("did not find author-email in grpc context"))
	}
	originalEmail := originalEmailArr[0]
	userMail, err := Decode64(originalEmail)
	if err != nil {
		return nil, httperrors.AuthError(ctx, fmt.Errorf("extract: non-base64 in author-email in grpc context %s", originalEmail))
	}
	originalNameArr := md.Get(HeaderUserName)
	if len(originalNameArr) == 0 {
		return nil, httperrors.AuthError(ctx, fmt.Errorf("extract: username undefined but mail defined in grpc context %s", userMail))
	}
	originalName := originalNameArr[0]
	userName, err := Decode64(originalName)
	if err != nil {
		return nil, httperrors.AuthError(ctx, fmt.Errorf("extract: non-base64 in author-username in grpc context %s", userName))
	}
	logger.FromContext(ctx).Info(fmt.Sprintf("Extract: original mail %s. Decoded: %s", originalEmail, userMail))
	logger.FromContext(ctx).Info(fmt.Sprintf("Extract: original name %s. Decoded: %s", originalName, userName))
	u := &User{
		Email: userMail,
		Name:  userName,
	}
	if u.Email == "" || u.Name == "" {
		return nil, httperrors.AuthError(ctx, errors.New("email and name in grpc context cannot both be empty"))
	}
	// RBAC Role of the user. only mandatory if DEX is enabled.
	if dexEnabled {
		rolesInHeader := md.Get(HeaderUserRole)
		if len(rolesInHeader) == 0 {
			return nil, httperrors.AuthError(ctx, fmt.Errorf("extract: role undefined but dex is enabled"))
		}
		userRole, err := Decode64(rolesInHeader[0])
		if err != nil {
			return nil, httperrors.AuthError(ctx, fmt.Errorf("extract: non-base64 in author-role in grpc context %s", userRole))
		}
		u.DexAuthContext = &DexAuthContext{
			Role: userRole,
		}
	}
	return u, nil
}

// ReadUserFromHttpHeader should only be used in the cd-service.
// ReadUserFromHttpHeader takes the User from the http request.
// It returns a User or an error if the user is not found.
func ReadUserFromHttpHeader(ctx context.Context, r *http.Request) (*User, error) {
	headerEmail, err := Decode64(r.Header.Get(HeaderUserEmail))
	if err != nil {
		return nil, httperrors.AuthError(ctx, errors.New("ExtractUserHttp: invalid data in email"))
	}
	headerName, err := Decode64(r.Header.Get(HeaderUserName))
	if err != nil {
		return nil, httperrors.AuthError(ctx, errors.New("ExtractUserHttp: invalid data in name"))
	}
	headerRole, err := Decode64(r.Header.Get(HeaderUserRole))
	if err != nil {
		return nil, httperrors.AuthError(ctx, errors.New("ExtractUserHttp: invalid data in role"))
	}

	if headerName != "" && headerEmail != "" {
		return &User{
			Email: headerEmail,
			Name:  headerName,
			DexAuthContext: &DexAuthContext{
				Role: headerRole,
			},
		}, nil
	}
	return nil, httperrors.AuthError(ctx, errors.New("ExtractUserHttp: did not find data in headers"))
}

// WriteUserToHttpHeader should only be used in the frontend-service
// WriteUserToHttpHeader writes the user into http headers
// it is used for requests like /release which are delegated from frontend-service to cd-service
func WriteUserToHttpHeader(r *http.Request, user User) {
	r.Header.Set(HeaderUserName, Encode64(user.Name))
	r.Header.Set(HeaderUserEmail, Encode64(user.Email))
}

// WriteUserRoleToHttpHeader should only be used in the frontend-service
// WriteUserRoleToHttpHeader writes the user role into http headers
// it is used for requests like /release and managing locks which are delegated from frontend-service to cd-service
func WriteUserRoleToHttpHeader(r *http.Request, role string) {
	r.Header.Set(HeaderUserRole, Encode64(role))
}

func GetUserOrDefault(u *User, defaultUser User) User {
	var userAdapted = User{
		Email: defaultUser.Email,
		Name:  defaultUser.Name,
	}
	if u != nil && u.Email != "" {
		userAdapted.Email = u.Email
		// if no username was specified, use email as username
		if u.Name == "" {
			userAdapted.Name = u.Email
		} else {
			userAdapted.Name = u.Name
		}
	}
	return userAdapted
}

type User struct {
	Email string
	Name  string
	// Optional. User role, only used if RBAC is enabled.
	DexAuthContext *DexAuthContext
}
