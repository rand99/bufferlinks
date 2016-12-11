FROM busybox
ADD linux-bufferlinks /
ENTRYPOINT ["/linux-bufferlinks"]
EXPOSE ":19870"
