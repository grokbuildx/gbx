'use strict';

// Node platform/architecture names and the corresponding release assets.
var platforms = [
  { os: 'darwin', arch: 'x64', asset: 'gbx-darwin-amd64' },
  { os: 'darwin', arch: 'arm64', asset: 'gbx-darwin-arm64' },
  { os: 'linux', arch: 'x64', asset: 'gbx-linux-amd64' },
  { os: 'linux', arch: 'arm64', asset: 'gbx-linux-arm64' },
  { os: 'win32', arch: 'x64', asset: 'gbx-win32-x64.exe' },
  { os: 'win32', arch: 'arm64', asset: 'gbx-win32-arm64.exe' },
];

platforms.forEach(function (platform) {
  platform.packageName = '@grokbuildx/gbx-' + platform.os + '-' + platform.arch;
  platform.binary = platform.os === 'win32' ? 'gbx.exe' : 'gbx';
});

function platformFor(os, arch) {
  return platforms.find(function (platform) { return platform.os === os && platform.arch === arch; });
}

module.exports = { platforms: platforms, platformFor: platformFor };
