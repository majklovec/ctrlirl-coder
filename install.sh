#!/usr/bin/env bash
core_pkgs=(
	# GStreamer pipeline contract: plugins for compositor/convert/encode/mux
	gstreamer1.0-tools
	gstreamer1.0-plugins-base # videoconvert, capsfilter, queue, ...
	gstreamer1.0-plugins-good # v4l2src, videorate, ...
	gstreamer1.0-plugins-ugly # x264enc
	gstreamer1.0-plugins-bad  # voaacenc, aacparse (audio track)
	gstreamer1.0-libav        # decodebin/uridecodebin inputs
	# browser layer audio: page audio lives on a PipeWire/Pulse null sink
	libpulse0        # Pulse/PipeWire client library
	pulseaudio-utils # pactl (per-layer null sink create/unload)
	pipewire-pulse   # audio session for headless Chrome
	ca-certificates
	chromium-codecs-ffmpeg-extra
)

sudo apt-get update || echo "warn: apt-get update failed (broken repo?); continuing with cached lists"
sudo apt-get install -y --no-install-recommends "${core_pkgs[@]}"
