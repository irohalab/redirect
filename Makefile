all:
	mkdir -p build
	env CGO_ENABLE=0 go build -o build/redirect .
