#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const zlib = require('zlib');
const { platforms } = require('../bin/platforms');

const launcherFiles = ['gbx.exe', 'install.js', 'wrapper.js', 'platforms.js'];
// gbx release binaries embed the open-source Grok Build (Apache-2.0); every
// package carries its license and third-party notices.
const grokNoticeFiles = ['GROK_BUILD_LICENSE', 'GROK_BUILD_THIRD_PARTY_NOTICES'];
const noticeFiles = ['LICENSE', 'THIRD_PARTY_NOTICES.md', ...grokNoticeFiles];

function releaseVersion(value) {
  const match = /^v?(\d+\.\d+\.\d+)(?:\.(\d+))?$/.exec(value || '');
  if (!match) throw new Error('Expected vMAJOR.MINOR.PATCH (or a legacy fourth component)');
  return match[1] + (match[2] ? '-' + match[2] : '');
}

function buildPackages(options) {
  const root = options.root || path.resolve(__dirname, '..');
  const binaries = options.binaries || path.join(root, 'dist');
  const output = options.output || path.join(root, 'dist', 'npm');
  const version = releaseVersion(options.version);
  const sourcePackage = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
  // Validate every artifact before leaving a partially prepared release.
  const binaryData = platforms.map(platform => zlib.gunzipSync(fs.readFileSync(path.join(binaries, platform.asset + '.gz'))));
  if (binaryData.some(data => !data.length)) throw new Error('An npm release binary is empty');
  const noticePath = file => grokNoticeFiles.includes(file) ? path.join(binaries, 'notices', file) : path.join(root, file);
  noticeFiles.forEach(file => { if (!fs.existsSync(noticePath(file))) throw new Error('Missing notice ' + noticePath(file)); });
  const optionalDependencies = {};
  platforms.forEach((platform, index) => {
    const directory = path.join(output, platform.os + '-' + platform.arch);
    fs.mkdirSync(path.join(directory, 'bin'), { recursive: true });
    fs.writeFileSync(path.join(directory, 'bin', platform.binary), binaryData[index], { mode: 0o755 });
    fs.writeFileSync(path.join(directory, 'package.json'), JSON.stringify({
      name: platform.packageName, version, description: 'GBX binary for ' + platform.os + '/' + platform.arch,
      os: [platform.os], cpu: [platform.arch], license: 'MIT AND Apache-2.0', repository: sourcePackage.repository,
      files: ['bin/', ...noticeFiles], publishConfig: { access: 'public' },
    }, null, 2) + '\n');
    noticeFiles.forEach(file => fs.copyFileSync(noticePath(file), path.join(directory, file)));
    optionalDependencies[platform.packageName] = version;
  });
  const main = path.join(output, 'main');
  fs.mkdirSync(path.join(main, 'bin'), { recursive: true });
  // bin/gbx.exe is the placeholder that bin/install.js replaces with the
  // native binary, so the installed command needs no Node.js on PATH.
  launcherFiles.forEach(file => fs.copyFileSync(path.join(root, 'bin', file), path.join(main, 'bin', file)));
  ['gbx.exe', 'wrapper.js', 'install.js'].forEach(file => fs.chmodSync(path.join(main, 'bin', file), 0o755));
  fs.copyFileSync(path.join(root, 'README.md'), path.join(main, 'README.md'));
  noticeFiles.forEach(file => fs.copyFileSync(noticePath(file), path.join(main, file)));
  fs.writeFileSync(path.join(main, 'package.json'), JSON.stringify({
    ...sourcePackage, version, description: 'Open-source multi-provider launcher for Grok Build and Codex, with Grok Build built in', optionalDependencies,
    scripts: { postinstall: 'node bin/install.js' },
    license: 'MIT AND Apache-2.0',
    files: [...launcherFiles.map(file => 'bin/' + file), 'README.md', ...noticeFiles],
  }, null, 2) + '\n');
  return { main, output, version };
}

module.exports = { buildPackages, releaseVersion, launcherFiles, grokNoticeFiles };
if (require.main === module) {
  try { const built = buildPackages({ version: process.argv[2] }); console.log('Prepared npm packages for ' + built.version); }
  catch (err) { console.error(err.message); process.exit(1); }
}
