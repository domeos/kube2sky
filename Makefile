# Copyright 2016 The Kubernetes Authors All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# MAINTAINER: rkasdf <rkasdfg@gmail.com>
# If you update this image please bump the tag value before pushing.

.PHONY: all kube2sky container push

TAG = 0.7
PREFIX = pub.domeos.org/domeos

all: kube2sky container push

kube2sky: kube2sky.go
	CGO_ENABLED=0 godep go build code.sohuno.com/domeos/kube2sky

container: kube2sky
	docker build -t $(PREFIX)/kube2sky:$(TAG) .

push:
	docker push $(PREFIX)/kube2sky:$(TAG)


