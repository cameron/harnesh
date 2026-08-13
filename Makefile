.PHONY: test go-test e2e nix-check

test: go-test e2e nix-check

go-test:
	go test ./...

e2e:
	./test/e2e.sh

nix-check:
	nix flake check path:.
