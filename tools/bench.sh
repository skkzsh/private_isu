#!/bin/bash
set -euvx

readonly app_ip_addr=192.168.0.42
# readonly app_ip_addr=133.242.234.199

cd /home/isucon/private_isu.git/benchmarker
./bin/benchmarker -u ./userdata -t http://${app_ip_addr} / |
tee ~/logs/bench-$(date +%Y%m%d-%H%M%S).log
