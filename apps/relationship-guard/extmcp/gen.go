// Package extmcp is agentgateway's ExtMCP guardrail protocol: ext_mcp.proto,
// unchanged from crates/protos/proto/ext_mcp.proto at agentgateway v1.5.0
// (github.com/agentgateway/agentgateway). The .pb.go files are generated from
// it and kept here, so a build needs no protoc. To regenerate, from
// apps/mcp-guard:
//
//	docker run --rm -v "$PWD":/src -w /src/extmcp golang:1.27-alpine sh -c '
//	  apk add --no-cache protobuf protobuf-dev >/dev/null &&
//	  go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 &&
//	  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2 &&
//	  M=Mext_mcp.proto=github.com/nickgamb/solo-lab/apps/mcp-guard/extmcp\;extmcp &&
//	  protoc -I . -I /usr/include --go_out=. --go_opt=paths=source_relative --go_opt=$M \
//	    --go-grpc_out=. --go-grpc_opt=paths=source_relative --go-grpc_opt=$M ext_mcp.proto'
package extmcp
