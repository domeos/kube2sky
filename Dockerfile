FROM pub.domeos.org/domeos/busybox:latest
MAINTAINER jackfan <jackfan@sohu-inc.com>

COPY kube2sky.go /
COPY kube2sky /
RUN chmod +x /kube2sky

CMD ["/kube2sky"]
