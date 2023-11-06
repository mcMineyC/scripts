#!/bin/sh
wget -O "minecraft.deb" "https://launcher.mojang.com/download/Minecraft.deb"
wget -O "steam.deb" "https://cdn.akamai.steamstatic.com/client/installer/steam.deb"
sudo apt install `pwd`/*.deb
rm minecraft.deb steam.deb -v