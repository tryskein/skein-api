// Package publicapi is the public REST API of skein-api: the HTTP server on
// its own port that serves /api/v1 to clients authenticating with a Skein API
// key.
//
// This file carries the swag v2 general API info of the public OpenAPI 3.1
// document. scripts/generate-public-openapi.sh generates the document from
// the annotations of this package into openapi/openapi.json; the file is
// committed and CI fails when it differs from a fresh generation.
//
// @title Skein public API
// @version 1
// @description Public REST API of Skein. Every request authenticates with an API key of a Skein user; the permission an operation requires is listed in its security requirement.
//
// @securityDefinitions.apikey SkeinApiKey
// @in header
// @name X-Skein-Api-Key
// @description API key of a Skein user (prefix ska_), created in the API keys section of the user's account.
package publicapi
