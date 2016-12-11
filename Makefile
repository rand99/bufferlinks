image:
	GOOS=linux go build -o linux-bufferlinks
	docker build . -t alexflint/bufferlinks
