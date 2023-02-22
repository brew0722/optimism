#!/bin/sh

cd /devnet/bin/

cp -f ./kscn_linux ./kscn
./kscnd start

tail -f ../nodedata/logs/kscnd.out
