#!/usr/bin/env node
// Postinstall for @grokbuildx/gbx: places the platform binary over the
// bin/gbx.exe placeholder, so `gbx` executes natively. GUI clients such as
// Xcode start agents without a PATH containing node, where a node launcher
// would exit before answering. Without this step (--ignore-scripts, pnpm,
// --omit=optional) the placeholder falls back to wrapper.js.
var fs = require('fs');
var path = require('path');
var crypto = require('crypto');
var zlib = require('zlib');
var platformFor = require('./platforms').platformFor;
var resolveBinary = require('./wrapper').resolveBinary;

var releaseURL = 'https://github.com/grokbuildx/gbx/releases/download/';
var maxDownload = 128 * 1024 * 1024;

// Link or copy beside dest, then rename over it: an interrupted install leaves
// either the placeholder or a complete binary.
function placeBinary(src, dest) {
    var staging = dest + '.install-' + process.pid;
    removeIfPresent(staging);
    try {
        try { fs.linkSync(src, staging); }
        catch (err) { fs.copyFileSync(src, staging); }
        if (process.platform !== 'win32') fs.chmodSync(staging, 493); // 0755
        fs.renameSync(staging, dest);
    } finally {
        removeIfPresent(staging);
    }
}

function removeIfPresent(file) {
    try { fs.unlinkSync(file); }
    catch (err) { if (err.code !== 'ENOENT') throw err; }
}

// install places the binary from the platform package; false when npm did not
// install that package.
function install(binDir, os, arch) {
    var source;
    try { source = resolveBinary(binDir, os, arch); }
    catch (err) { return false; }
    placeBinary(source, path.join(binDir, 'gbx.exe'));
    return true;
}

// releaseTag inverts build-npm-packages.js releaseVersion: a release tag is
// the npm version with a leading v (1.6.10 -> v1.6.10, 1.0.0-0 -> v1.0.0-0).
function releaseTag(version) {
    if (!/^\d+\.\d+\.\d+(?:-\d+)?$/.test(version || '')) throw new Error('cannot derive a release tag from version ' + version);
    return 'v' + version;
}

function get(url, redirects) {
    var start = new URL(url);
    return new Promise(function (resolve, reject) {
        var client = require(start.protocol === 'http:' ? 'http' : 'https');
        var req = client.get(start, { headers: { 'user-agent': 'gbx-npm-install' } }, function (res) {
            if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
                res.resume();
                var next = new URL(res.headers.location, start);
                // GitHub redirects to its asset CDN; never downgrade to plain HTTP.
                if ((redirects || 0) >= 5 || (next.protocol !== 'https:' && next.protocol !== start.protocol)) {
                    reject(new Error('refusing redirect to ' + next.origin));
                    return;
                }
                resolve(get(next.href, (redirects || 0) + 1));
                return;
            }
            if (res.statusCode !== 200) {
                res.resume();
                reject(new Error('HTTP ' + res.statusCode + ' for ' + start.href));
                return;
            }
            var chunks = [];
            var size = 0;
            res.on('data', function (chunk) {
                size += chunk.length;
                if (size > maxDownload) { req.destroy(new Error('download exceeds ' + maxDownload + ' bytes')); return; }
                chunks.push(chunk);
            });
            res.on('end', function () { resolve(Buffer.concat(chunks)); });
            res.on('error', reject);
        });
        req.setTimeout(60000, function () { req.destroy(new Error('timed out fetching ' + start.href)); });
        req.on('error', reject);
    });
}

// download fetches the release asset that the platform package was built
// from, verifies it against the release's SHA256SUMS and places it. npm can
// serve a new launcher minutes before it serves the platform packages, and
// skips an optional dependency it cannot resolve without failing the install.
function download(binDir, os, arch, version, base) {
    var platform = platformFor(os, arch);
    if (!platform) return Promise.reject(new Error('Unsupported platform: ' + os + ' ' + arch));
    var root = (base || releaseURL).replace(/\/?$/, '/') + releaseTag(version) + '/';
    var asset = platform.asset + '.gz';
    return Promise.all([get(root + 'SHA256SUMS'), get(root + asset)]).then(function (results) {
        var want = results[0].toString('utf8').split(/\r?\n/).map(function (line) {
            return line.trim().split(/\s+\*?/);
        }).filter(function (fields) { return fields[1] === asset; }).map(function (fields) { return fields[0]; })[0];
        if (!want) throw new Error('SHA256SUMS has no entry for ' + asset);
        var got = crypto.createHash('sha256').update(results[1]).digest('hex');
        if (got !== want.toLowerCase()) throw new Error(asset + ' checksum mismatch');
        var dest = path.join(binDir, 'gbx.exe');
        var staging = dest + '.download-' + process.pid;
        try {
            fs.writeFileSync(staging, zlib.gunzipSync(results[1]));
            if (process.platform !== 'win32') fs.chmodSync(staging, 493); // 0755
            fs.renameSync(staging, dest);
        } finally {
            removeIfPresent(staging);
        }
    });
}

function packageVersion(binDir) {
    return JSON.parse(fs.readFileSync(path.join(path.dirname(binDir), 'package.json'), 'utf8')).version;
}

module.exports = { install: install, placeBinary: placeBinary, download: download, releaseTag: releaseTag };
if (require.main === module) {
    var prefix = '[@grokbuildx/gbx postinstall] ';
    var placed;
    try { placed = install(__dirname, process.platform, process.arch); }
    catch (err) {
        console.error(prefix + 'Failed to place binary: ' + err.message);
        process.exitCode = 1;
    }
    if (placed === false) {
        var version;
        Promise.resolve().then(function () {
            version = packageVersion(__dirname);
            console.error(prefix + 'Platform package not installed; downloading ' + releaseTag(version) + ' from GitHub Releases.');
            return download(__dirname, process.platform, process.arch, version, process.env.GBX_RELEASE_DOWNLOAD_URL);
        }).catch(function (err) {
            // A failed postinstall must fail the install: exiting 0 here leaves
            // a gbx command that only reports a missing binary on first use.
            console.error(prefix + 'Download failed: ' + err.message);
            console.error('  The npm platform package normally becomes available a few minutes after a release;');
            console.error('  install again then, without uninstalling first:');
            console.error('    npm install -g @grokbuildx/gbx@' + (version || 'latest'));
            process.exitCode = 1;
        });
    }
}
