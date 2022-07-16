#!/bin/bash
set -euvx

sudo truncate -s 0 -c /var/log/nginx/access.log
sudo truncate -s 0 -c /var/log/nginx/error.log
sudo truncate -s 0 -c /var/log/mysql/mysql-slow.log
sudo truncate -s 0 -c /var/log/mysql/error.log
# mysqladmin flush-logs

cd /home/isucon/private_isu/webapp/golang
make
sudo systemctl restart isu-go

sudo systemctl restart mysql
sudo systemctl restart nginx

