#!/bin/bash
set -euvx

sudo cat /var/log/nginx/access.log |
	alp json -q  --qs-ignore-values --sort avg -r -m '^/posts/[0-9]*.$','^/@[a-z]*$','^/image/[0-9]*.jpg$','^/image/[0-9]*.png$','^/image/[0-9]*.gif$' |
	tee ~/log/alp-$(date +%Y%m%d-%H%M%S).log

# sudo mysqldumpslow /var/log/mysql/mysql-slow.log | tee ~/log/slow-$(date +%Y%m%d-%H%M%S).log
# sudo pt-query-digest /var/log/mysql/mysql-slow.log | tee ~/log/pt-query-digest-$(date +%Y%m%d-%H%M%S).log

# go tool pprof -http=:10060 http://localhost:6060/debug/pprof/profile

