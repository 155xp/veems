#!/bin/sh
# Runs once inside the build VM for `veems build linux --desktop`: Xfce on
# Xorg :0, Chromium, and auto-login as user "agent" so a desktop is up at boot.
set -eux
export DEBIAN_FRONTEND=noninteractive
# The cloud kernel has no graphics drivers; the generic one does.
apt-get install -y --no-install-recommends linux-image-amd64 \
  xserver-xorg-core xserver-xorg-input-libinput xserver-xorg-legacy x11-xserver-utils xauth dbus-x11 \
  lightdm lightdm-gtk-greeter xfce4 xfce4-terminal xfwm4 xfdesktop4 xfce4-panel thunar mousepad \
  chromium fonts-dejavu-core fonts-noto-color-emoji adwaita-icon-theme xdotool wmctrl scrot at-spi2-core
dpkg -l | awk '/^ii +linux-image-.*cloud/ {print $2}' | xargs -r apt-get purge -y || true
rm -f /boot/vmlinuz-*cloud* /boot/initrd.img-*cloud* /boot/config-*cloud* /boot/System.map-*cloud*

useradd -m -s /bin/bash -G sudo agent
echo 'agent ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/agent
groupadd -r autologin || true
gpasswd -a agent autologin
mkdir -p /etc/lightdm/lightdm.conf.d /work
chown agent:agent /work
cat > /etc/lightdm/lightdm.conf.d/50-veems.conf <<'CONF'
[Seat:*]
autologin-user=agent
autologin-user-timeout=0
autologin-session=xfce
user-session=xfce
xserver-command=X -s 0 -dpms
CONF
# Fixed size, never blank or lock.
cat > /etc/xdg/autostart/veems-screen.desktop <<'CONF'
[Desktop Entry]
Type=Application
Name=veems screen
Exec=sh -c "xrandr -s 1280x800; xset s off -dpms"
CONF
# Skip Xfce's first-run panel question.
mkdir -p /etc/xdg/xfce4/xfconf/xfce-perchannel-xml
cp /etc/xdg/xfce4/panel/default.xml /etc/xdg/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml
# Chromium as the default browser, without first-run or keyring prompts.
mkdir -p /etc/chromium.d
echo 'export CHROMIUM_FLAGS="$CHROMIUM_FLAGS --no-first-run --password-store=basic --disable-features=Translate"' > /etc/chromium.d/veems
update-alternatives --set x-www-browser /usr/bin/chromium || true
systemctl set-default graphical.target
systemctl enable lightdm
update-grub
