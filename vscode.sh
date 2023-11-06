#!/bin/sh
wget -O code-insiders.deb "https://code.visualstudio.com/sha/download?build=insider&os=linux-deb-x64"
sudo apt install `pwd`/code-insiders.deb -y