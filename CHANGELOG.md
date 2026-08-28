# Changelog

## [1.23.0](https://github.com/advoq/civm/compare/v1.22.36...v1.23.0) (2026-08-28)


### Features

* initial open-source release of civm ([40d579d](https://github.com/advoq/civm/commit/40d579d324b1445b17c3bc435975c10a9ec4a15c))

## [1.22.36](https://github.com/emersonbusson/civm/compare/v1.22.35...v1.22.36) (2026-08-09)


### Bug Fixes

* **ci:** harden reruns and upgrade checkout to Node 24 ([65f0641](https://github.com/emersonbusson/civm/commit/65f0641aceb51aad6b29834cfc2cadd390ed6891))
* **ci:** keep fork code off self-hosted smoke ([#273](https://github.com/emersonbusson/civm/issues/273)) ([68c0f59](https://github.com/emersonbusson/civm/commit/68c0f59d1e125dda4db08a9b9643df9fe2d6a240))
* **runner:** classify transient action download failures ([13e7dbb](https://github.com/emersonbusson/civm/commit/13e7dbb817a2b1d41b3c98835346428de0f8c310))


### CI

* upgrade checkout actions to Node 24 ([5de0bc1](https://github.com/emersonbusson/civm/commit/5de0bc1c04cba250e60307753be9f7fc3ce9393f))
* upgrade setup-go to Node 24 ([#275](https://github.com/emersonbusson/civm/issues/275)) ([cadc2e9](https://github.com/emersonbusson/civm/commit/cadc2e94151c10a8eb6ab70c59edb8d1d089f5c7))

## [1.22.35](https://github.com/emersonbusson/civm/compare/v1.22.34...v1.22.35) (2026-08-06)


### Bug Fixes

* **host:** standardize VM memory at 12 GiB ([#265](https://github.com/emersonbusson/civm/issues/265)) ([f452e1b](https://github.com/emersonbusson/civm/commit/f452e1b7be2afba617c1dd7a48ce7af3ffad65eb))


### Documentation

* **orchestrator:** preserve Go port design history ([#268](https://github.com/emersonbusson/civm/issues/268)) ([aaab870](https://github.com/emersonbusson/civm/commit/aaab8702b0cf3a585fd65d00078e98fcd7cdab7d))

## [1.22.34](https://github.com/emersonbusson/civm/compare/v1.22.33...v1.22.34) (2026-08-05)


### Bug Fixes

* **runner:** preserve process arrays ([7024aef](https://github.com/emersonbusson/civm/commit/7024aef4a43b2bd6acab9c33268b2013c24f5fb5))

## [1.22.33](https://github.com/emersonbusson/civm/compare/v1.22.32...v1.22.33) (2026-08-05)


### Bug Fixes

* **runner:** await gate session convergence ([#259](https://github.com/emersonbusson/civm/issues/259)) ([bf46a13](https://github.com/emersonbusson/civm/commit/bf46a1356c8bc7bf802ec3079045158d7b7f870e))

## [1.22.32](https://github.com/emersonbusson/civm/compare/v1.22.31...v1.22.32) (2026-08-05)


### Bug Fixes

* **runner:** catch ACL probe denial ([#256](https://github.com/emersonbusson/civm/issues/256)) ([8ad0f21](https://github.com/emersonbusson/civm/commit/8ad0f21c8c567f893903c8a8625225370743140b))

## [1.22.31](https://github.com/emersonbusson/civm/compare/v1.22.30...v1.22.31) (2026-08-05)


### Bug Fixes

* **runner:** require effective ACL mutation ([#253](https://github.com/emersonbusson/civm/issues/253)) ([3458352](https://github.com/emersonbusson/civm/commit/34583520ab1d089eed8aa7d9de8b69cc334c95f9))

## [1.22.30](https://github.com/emersonbusson/civm/compare/v1.22.29...v1.22.30) (2026-08-05)


### Bug Fixes

* **runner:** validate rollback physical targets ([#250](https://github.com/emersonbusson/civm/issues/250)) ([5929e35](https://github.com/emersonbusson/civm/commit/5929e352359689c67afe58b428f37cbc0e5c79cf))

## [1.22.29](https://github.com/emersonbusson/civm/compare/v1.22.28...v1.22.29) (2026-08-05)


### Bug Fixes

* **runner:** validate relocated rollback junctions ([#247](https://github.com/emersonbusson/civm/issues/247)) ([25723aa](https://github.com/emersonbusson/civm/commit/25723aadd105334cad97b4230fc0cdc27747e1dd))

## [1.22.28](https://github.com/emersonbusson/civm/compare/v1.22.27...v1.22.28) (2026-08-05)


### Bug Fixes

* **runner:** inspect restricted junctions by handle ([#244](https://github.com/emersonbusson/civm/issues/244)) ([c368595](https://github.com/emersonbusson/civm/commit/c36859528257fb841579c7f9c174c9d38ab7e9b1))

## [1.22.27](https://github.com/emersonbusson/civm/compare/v1.22.26...v1.22.27) (2026-08-05)


### Bug Fixes

* **runner:** restore DACL writes for legacy owners ([#241](https://github.com/emersonbusson/civm/issues/241)) ([35522f1](https://github.com/emersonbusson/civm/commit/35522f1e8f53a8fc734e515591d3549c7b6f44b4))

## [1.22.26](https://github.com/emersonbusson/civm/compare/v1.22.25...v1.22.26) (2026-08-05)


### Bug Fixes

* recover legacy gate ACLs ([#238](https://github.com/emersonbusson/civm/issues/238)) ([ef29b59](https://github.com/emersonbusson/civm/commit/ef29b59170ffb33f6bd93b8f8bc1936560b6aac2))

## [1.22.25](https://github.com/emersonbusson/civm/compare/v1.22.24...v1.22.25) (2026-08-05)


### Bug Fixes

* allow only pinned runner junctions ([#235](https://github.com/emersonbusson/civm/issues/235)) ([b51dba8](https://github.com/emersonbusson/civm/commit/b51dba835ae4b5b4fe98df48c8543a9eddce2557))

## [1.22.24](https://github.com/emersonbusson/civm/compare/v1.22.23...v1.22.24) (2026-08-05)


### Bug Fixes

* isolate Windows gate runners ([#232](https://github.com/emersonbusson/civm/issues/232)) ([267ded0](https://github.com/emersonbusson/civm/commit/267ded09e894ed34e69b34e67f29eed28b015432))

## [1.22.23](https://github.com/emersonbusson/civm/compare/v1.22.22...v1.22.23) (2026-08-05)


### Bug Fixes

* **ci:** enforce clean generation boundary ([59cb68e](https://github.com/emersonbusson/civm/commit/59cb68e257dabd014bfdd9d0ab5be9910a9a1f07))


### Documentation

* **host:** record clean boundary rollout ([#230](https://github.com/emersonbusson/civm/issues/230)) ([068cf37](https://github.com/emersonbusson/civm/commit/068cf37b3befa2fad325648996db1100f789e24a))

## [1.22.22](https://github.com/emersonbusson/civm/compare/v1.22.21...v1.22.22) (2026-08-03)


### Bug Fixes

* **runner:** resolve org fleet before no-repos ([592cfec](https://github.com/emersonbusson/civm/commit/592cfecdce88c7f71bca3630d3d213242eb8290a))
* **runner:** resolve org fleet before no-repos ([50f3fde](https://github.com/emersonbusson/civm/commit/50f3fde85f284ebb5b31f35ce3487d20cce47974)), closes [#221](https://github.com/emersonbusson/civm/issues/221)

## [1.22.21](https://github.com/emersonbusson/civm/compare/v1.22.20...v1.22.21) (2026-08-02)


### Bug Fixes

* **runner:** dwell and gracefully restart watchdog ([a7cc16b](https://github.com/emersonbusson/civm/commit/a7cc16b3b15baa120772d2816fe86b275a5d43e1))
* **runner:** evitar restart falso no pos-job ([6d59ed4](https://github.com/emersonbusson/civm/commit/6d59ed4386cf515ab474eb8f4095a0f60718f627))
* **runner:** preserve both cgroup errors ([6e2fda4](https://github.com/emersonbusson/civm/commit/6e2fda4d45a5a5ec209a7dcddac11378b73a1e03))
* **runner:** satisfy lint for watchdog recovery ([f545fc9](https://github.com/emersonbusson/civm/commit/f545fc9ca06cb7427777e1af893abc2e0c8157b4))

## [1.22.20](https://github.com/emersonbusson/civm/compare/v1.22.19...v1.22.20) (2026-08-02)


### Bug Fixes

* **runner:** purge orphan listeners before restart ([f646496](https://github.com/emersonbusson/civm/commit/f6464967b5f21a984e506a134c7b368decef5ede))
* **runner:** purge orphan listeners before restart ([3520859](https://github.com/emersonbusson/civm/commit/35208594cd0d80ac3f3502121e79f04e7ad3fd9f)), closes [#213](https://github.com/emersonbusson/civm/issues/213)

## [1.22.19](https://github.com/emersonbusson/civm/compare/v1.22.18...v1.22.19) (2026-08-02)


### Bug Fixes

* preserve SSH stderr during VHDX drain ([5d6a6bd](https://github.com/emersonbusson/civm/commit/5d6a6bd4038d9ecad8c885198e83288ba16f5fe0))
* preserve SSH stderr during VHDX drain ([13e45b4](https://github.com/emersonbusson/civm/commit/13e45b4ce926b2f0a0f8761037d0cdee66a17128)), closes [#210](https://github.com/emersonbusson/civm/issues/210)

## [1.22.18](https://github.com/emersonbusson/civm/compare/v1.22.17...v1.22.18) (2026-08-02)


### Bug Fixes

* purge Playwright cache at idle boundaries ([a6bfb15](https://github.com/emersonbusson/civm/commit/a6bfb1538cc95f38cc1bd2460dc895334f703bed))
* purge Playwright cache at idle boundaries ([11699f3](https://github.com/emersonbusson/civm/commit/11699f328ddd5f344afb74e7e36719543ac81f40)), closes [#207](https://github.com/emersonbusson/civm/issues/207)

## [1.22.17](https://github.com/emersonbusson/civm/compare/v1.22.16...v1.22.17) (2026-08-02)


### Bug Fixes

* purge hot caches at idle boundaries ([04a1b6a](https://github.com/emersonbusson/civm/commit/04a1b6a4f34ce14ed9755d1018859828399b80ac))
* purge hot caches at idle boundaries ([5a3dc71](https://github.com/emersonbusson/civm/commit/5a3dc717154de8eda50d234adcf2dcec2accde87))
* reap completed compose containers ([0f64185](https://github.com/emersonbusson/civm/commit/0f64185e5a608ae247a68454f6465ab6eb004027)), closes [#205](https://github.com/emersonbusson/civm/issues/205)

## [1.22.16](https://github.com/emersonbusson/civm/compare/v1.22.15...v1.22.16) (2026-08-02)


### Bug Fixes

* restore Docker clean slate after idle jobs ([34bf4e9](https://github.com/emersonbusson/civm/commit/34bf4e913e81ed537e59b8f8f53e7a153201def5))
* restore Docker clean slate after idle jobs ([014758b](https://github.com/emersonbusson/civm/commit/014758b7522f62af51190eea4b56c26d4c9817d7))

## [1.22.15](https://github.com/emersonbusson/civm/compare/v1.22.14...v1.22.15) (2026-08-02)


### Bug Fixes

* **watchdog:** fence runner before restart ([921e63a](https://github.com/emersonbusson/civm/commit/921e63aaadcd10a70ec6691c5d28cf466de33d6b))
* **watchdog:** fence runner before restart ([7fb1c90](https://github.com/emersonbusson/civm/commit/7fb1c90186d9f0896e45d9926fd25ecd543b79ad))

## [1.22.14](https://github.com/emersonbusson/civm/compare/v1.22.13...v1.22.14) (2026-08-02)


### Bug Fixes

* **reaper:** accept completed workflow race ([1f28b30](https://github.com/emersonbusson/civm/commit/1f28b30bd71e5780fd8fec01ba0e770680098b30))
* **reaper:** accept completed workflow race ([3272b5c](https://github.com/emersonbusson/civm/commit/3272b5c1dca8f8c98bd420c074d9a54a73feb552))

## [1.22.13](https://github.com/emersonbusson/civm/compare/v1.22.12...v1.22.13) (2026-08-02)


### Bug Fixes

* reuse configured GitHub CLI authentication ([#192](https://github.com/emersonbusson/civm/issues/192)) ([19279fd](https://github.com/emersonbusson/civm/commit/19279fd4aa30a97abc93a89c14adc8bce23f9051))

## [1.22.12](https://github.com/emersonbusson/civm/compare/v1.22.11...v1.22.12) (2026-08-02)


### Bug Fixes

* drain organization runner labels safely ([#190](https://github.com/emersonbusson/civm/issues/190)) ([1afaa42](https://github.com/emersonbusson/civm/commit/1afaa42523f0c3d49d84450dd161011d78e19f20))

## [1.22.11](https://github.com/emersonbusson/civm/compare/v1.22.10...v1.22.11) (2026-08-02)


### Bug Fixes

* **runner:** honor maintenance drain in watchdog ([#187](https://github.com/emersonbusson/civm/issues/187)) ([c475662](https://github.com/emersonbusson/civm/commit/c475662bc59cb6ef566a435efe975206e438c7b3))

## [1.22.10](https://github.com/emersonbusson/civm/compare/v1.22.9...v1.22.10) (2026-08-02)


### Bug Fixes

* avoid watchdog restart race with jobs ([f9a2e8a](https://github.com/emersonbusson/civm/commit/f9a2e8a1b865132b332169ad4d32773c46a586d6))
* cap external yarn berry caches ([d3fd80c](https://github.com/emersonbusson/civm/commit/d3fd80cc608cc3e31fd520bec39f03a581d30bee))
* **host:** monitor the active orchestrator owner ([#185](https://github.com/emersonbusson/civm/issues/185)) ([9a0b997](https://github.com/emersonbusson/civm/commit/9a0b99752e4e674dbc0f1bf9eec84e301a9a3bfd))
* **queue:** recover closed PR runs and runner state ([#179](https://github.com/emersonbusson/civm/issues/179)) ([01e0f6c](https://github.com/emersonbusson/civm/commit/01e0f6c366fc25f44dc1ce91bf324462c7e7aafe))
* reap orphan testcontainers after OOM ([23d606a](https://github.com/emersonbusson/civm/commit/23d606a5ac9dd8e6820438af0db4c4ddc7db098f))
* recover org runner without inferred repos ([e2be67f](https://github.com/emersonbusson/civm/commit/e2be67fc4464573e67e22bab759528b05de677c6))
* recover stale busy org runner ([1b60423](https://github.com/emersonbusson/civm/commit/1b604239997dac3f02e61ab51d0114ed0975a358))
* report org runner availability without repos ([f146766](https://github.com/emersonbusson/civm/commit/f1467665a0c2c7d92660890885bac9a6926fb123))
* resolve runner user for admit probes ([f87a002](https://github.com/emersonbusson/civm/commit/f87a002c9e3d5a3724b0bc567af0dee78de2ccc6))
* **runner:** recover stalled sessions and clean managed volumes ([#183](https://github.com/emersonbusson/civm/issues/183)) ([58a106c](https://github.com/emersonbusson/civm/commit/58a106c0fd9f491cdcc9fcd83f7fc918c9c3fc98))
* tolerate missing change-detection base ([4f220c8](https://github.com/emersonbusson/civm/commit/4f220c8fba8a27085b01a130e49236e82d388ad0))

## [1.22.9](https://github.com/emersonbusson/civm/compare/v1.22.8...v1.22.9) (2026-07-15)


### Bug Fixes

* **orchestrator:** survive host power interruptions ([#176](https://github.com/emersonbusson/civm/issues/176)) ([ce677ed](https://github.com/emersonbusson/civm/commit/ce677edb308ed3cad63991c243ea02ebfef3db2c))

## [1.22.8](https://github.com/emersonbusson/civm/compare/v1.22.7...v1.22.8) (2026-07-14)


### Bug Fixes

* **orchestrator:** quiet expected 403 on multi-repo tip commits ([#174](https://github.com/emersonbusson/civm/issues/174)) ([d3300f3](https://github.com/emersonbusson/civm/commit/d3300f3634ae80fe0315d2c18196dcf436f98304))

## [1.22.7](https://github.com/emersonbusson/civm/compare/v1.22.6...v1.22.7) (2026-07-14)


### Bug Fixes

* **metrics:** escape vmState in log string; quiet tip 404 ([#173](https://github.com/emersonbusson/civm/issues/173)) ([fdc0528](https://github.com/emersonbusson/civm/commit/fdc05281beffd9446edf2c2df67565732013887b))
* **orchestrator:** treat VM Off as no active guest job ([#172](https://github.com/emersonbusson/civm/issues/172)) ([44d321a](https://github.com/emersonbusson/civm/commit/44d321a3aa535d2d9c7ff401ec433d741b242474))


### Documentation

* host orchestrator setup runbook and supersede Go host port ([#170](https://github.com/emersonbusson/civm/issues/170)) ([93cb600](https://github.com/emersonbusson/civm/commit/93cb60041f60f1d3b82bf2e7782fa9d841719f16))

## [1.22.6](https://github.com/emersonbusson/civm/compare/v1.22.5...v1.22.6) (2026-07-14)


### Bug Fixes

* **orchestrator:** drive PR queue from Repos and retry Mount-VHD ([#168](https://github.com/emersonbusson/civm/issues/168)) ([76ef1bf](https://github.com/emersonbusson/civm/commit/76ef1bf605b3bba692902bbca98e1b531a544457))

## [1.22.5](https://github.com/emersonbusson/civm/compare/v1.22.4...v1.22.5) (2026-07-14)


### CI

* add gitleaks full-history secret scan ([#166](https://github.com/emersonbusson/civm/issues/166)) ([c587d64](https://github.com/emersonbusson/civm/commit/c587d6407c3dbea919a1a4607d861d532a55ddcd))

## [1.22.4](https://github.com/emersonbusson/civm/compare/v1.22.3...v1.22.4) (2026-07-10)


### Bug Fixes

* **deploy:** skip no-op copy when activate runs in-place ([60a6982](https://github.com/emersonbusson/civm/commit/60a6982bb43c285c2946c81c26f01fd640bf9c0d))
* **orchestrator:** push-wave before admit + reject stale host metrics ([fa713ef](https://github.com/emersonbusson/civm/commit/fa713effcbb7bc755da7aa3c8424a458abbe448a))
* **orchestrator:** push-wave before admit + reject stale host metrics ([ff8212f](https://github.com/emersonbusson/civm/commit/ff8212f180545341069bce5bb085859559028d62))

## [1.22.3](https://github.com/emersonbusson/civm/compare/v1.22.2...v1.22.3) (2026-07-10)


### Bug Fixes

* **orchestrator:** push-wave seed always + paid-CI clean on tip change ([70903a1](https://github.com/emersonbusson/civm/commit/70903a1c32ca62c41b12d4fdf9083d6235cbe08c))
* **orchestrator:** push-wave seed always + paid-CI clean on tip change ([a2647b5](https://github.com/emersonbusson/civm/commit/a2647b512b69c7e0e693e9dfc2a89ca157f653de))
* **orchestrator:** push-wave seed always + paid-CI clean on tip change ([#160](https://github.com/emersonbusson/civm/issues/160)) ([70903a1](https://github.com/emersonbusson/civm/commit/70903a1c32ca62c41b12d4fdf9083d6235cbe08c))

## [1.22.2](https://github.com/emersonbusson/civm/compare/v1.22.1...v1.22.2) (2026-07-09)


### Bug Fixes

* **orchestrator:** compact VHDX between pushes of the same PR ([#157](https://github.com/emersonbusson/civm/issues/157)) ([360dd7a](https://github.com/emersonbusson/civm/commit/360dd7a7934bbfd1f40dfff19c9b671d69956c10))
* **queue:** close disk-admission and guest-liveness reconciliation gaps ([#155](https://github.com/emersonbusson/civm/issues/155)) ([3ffa617](https://github.com/emersonbusson/civm/commit/3ffa617d786f806c1a0657a8c37c2ce39c002ec1))
* **reaper:** reap superseded SHA runs on open PRs ([#156](https://github.com/emersonbusson/civm/issues/156)) ([9ac05be](https://github.com/emersonbusson/civm/commit/9ac05be65d8a6beb19cdb9256ff072902601bafd))


### Documentation

* reconcile disk-floor refs (AdmitFloorGB 51-&gt;55) + Kahneman path + supersede banners ([#151](https://github.com/emersonbusson/civm/issues/151)) ([68dc758](https://github.com/emersonbusson/civm/commit/68dc758197555028c14973269425d3cbffa9565f))
* **validation:** record superseded-sha reaper live on guest ([3b6aefc](https://github.com/emersonbusson/civm/commit/3b6aefc9d0f6b8f9165186998abb150ece26ab4d))

## [1.22.1](https://github.com/emersonbusson/civm/compare/v1.22.0...v1.22.1) (2026-06-27)


### Documentation

* **specs:** land SSDV3 spec for resilient guest access (serial console OOB) ([#148](https://github.com/emersonbusson/civm/issues/148)) ([6f49fd6](https://github.com/emersonbusson/civm/commit/6f49fd6bacfad345f8691fe850e1c1a2f0ad7406))
* **specs:** land SSDV3 specs for ephemeral clean-slate CI + V: disk budget ([#147](https://github.com/emersonbusson/civm/issues/147)) ([27cde85](https://github.com/emersonbusson/civm/commit/27cde85024939921c37e3a3f6a43d29c0f309604))

## [1.22.0](https://github.com/emersonbusson/civm/compare/v1.21.0...v1.22.0) (2026-06-27)


### Features

* **civm:** PR-queue FIFO gate + disk-gate boundary hardening ([#145](https://github.com/emersonbusson/civm/issues/145)) ([cb19f97](https://github.com/emersonbusson/civm/commit/cb19f9772596d893c50b7516c369f573818af2f6))

## [1.21.0](https://github.com/emersonbusson/civm/compare/v1.20.0...v1.21.0) (2026-06-19)


### Features

* **devctl:** add boundary_compact to reclaim VHDX in PR gaps ([#144](https://github.com/emersonbusson/civm/issues/144)) ([b64c834](https://github.com/emersonbusson/civm/commit/b64c834b7fd318f830fedc2e71964c19f3fb9c60))
* **orchestrator:** boundary_compact + deep-clean to hold CI disk floor ([#140](https://github.com/emersonbusson/civm/issues/140)) ([6682f07](https://github.com/emersonbusson/civm/commit/6682f07f6c8ef16ab2743fb574cd8a6d7adeeec6))
* **runner:** codify durable acme runner serialization (remove, not disable) ([#141](https://github.com/emersonbusson/civm/issues/141)) ([fbf1565](https://github.com/emersonbusson/civm/commit/fbf156535df4f5bec907f85c4315889458fdd12d))


### Bug Fixes

* **hook:** cut CI disk source — V: drain metering + orphan/image reap ([#143](https://github.com/emersonbusson/civm/issues/143)) ([9c34ff2](https://github.com/emersonbusson/civm/commit/9c34ff26a15863a01f49e597a6b937bb2c55203f))

## [1.20.0](https://github.com/emersonbusson/civm/compare/v1.19.0...v1.20.0) (2026-06-18)


### Features

* **orchestrator:** gate VM start on 51GB free both sides ([8b809ce](https://github.com/emersonbusson/civm/commit/8b809cee863f32e7f24c6aa8b65919a2f11fb367))
* **orchestrator:** scale-to-zero disk-safety with 2-phase gate ([bba7b05](https://github.com/emersonbusson/civm/commit/bba7b05b3116b419cf0081ce97e1d896cabc6e53))
* **orchestrator:** scale-to-zero disk-safety with 2-phase reclaim gate ([5f22796](https://github.com/emersonbusson/civm/commit/5f227967a385617f4668e51cf7fb8d755dd2a3ed))


### Bug Fixes

* **infra:** guard host-metrics Get-VHD with timeout and task limit ([9569ac6](https://github.com/emersonbusson/civm/commit/9569ac66f20b8fb8f831b0fe51505020048ced29))
* **orchestrator:** gate disk-safety on hasWork; -Observe non-mutating ([3d90115](https://github.com/emersonbusson/civm/commit/3d90115750c06b397c60323f8df6a5c1e3deb0b1))
* **orchestrator:** monitor emersonbusson/civm so the box runs its own CI ([21f235c](https://github.com/emersonbusson/civm/commit/21f235c2cf06639f13941b2bf4e45cd1cc37994f))
* **orchestrator:** tr -dc 0-9 in guest-clean free probe ([26c81aa](https://github.com/emersonbusson/civm/commit/26c81aa88282c308dd0405479889be47eb8ba1ae))


### Documentation

* dedup validation.md rule and pointers ([de3c3e0](https://github.com/emersonbusson/civm/commit/de3c3e0026733e609b5e17d5d97fdfaabec0324e))
* **orchestrator:** scale-to-zero SPEC + supersede legacy reclaim ([a731f16](https://github.com/emersonbusson/civm/commit/a731f166f35fdc042c2eb71c25d2403d8943f5eb))

## [1.19.0](https://github.com/emersonbusson/civm/compare/v1.18.7...v1.19.0) (2026-06-17)


### Features

* **civm:** scale-to-zero orchestrator + prune age-guard + parity docs ([0e97a12](https://github.com/emersonbusson/civm/commit/0e97a12d24844b058006862ce0fa8c239ce599b9))
* **disk:** frequent build-cache prune timer, no heavy-lock defer ([026aa4e](https://github.com/emersonbusson/civm/commit/026aa4ee99098899d92e269a75c6acd8a0e4ff23))
* **disk:** prune finished-run service images in the 3min hygiene timer ([42f2b78](https://github.com/emersonbusson/civm/commit/42f2b780a5238010f5d17f05e5c8ef20f03ac7ea))
* **orchestrator:** scale-to-zero VM orchestrator with tested decision module ([533d1bf](https://github.com/emersonbusson/civm/commit/533d1bfa54ed5d32d60981ae7fd34a4f2f119a7d))
* **vm:** scale-to-zero VM orchestrator + vm.md inventory ([1c70e22](https://github.com/emersonbusson/civm/commit/1c70e22f32d7618f98662ecbee370e05a5d72bba))
* **vm:** scope orchestrator to acme token + SSH stop-guard ([7f6dc69](https://github.com/emersonbusson/civm/commit/7f6dc69cbb0a39544940ac906d4386024d8438c5))


### Bug Fixes

* **ci:** age-guard per-run image prune to stop evicting active runs ([98c7557](https://github.com/emersonbusson/civm/commit/98c7557325ca6e311dcc246771d41544133b4b71))
* **cleanup:** drop -a from idle docker prune to stop image race ([811b435](https://github.com/emersonbusson/civm/commit/811b4359c628f0edae8a94c571822af016e2e0e6))
* **cleanup:** drop -a image prune from busy branch (vendor-date race) ([7e9cc0d](https://github.com/emersonbusson/civm/commit/7e9cc0d5bb880428470029ba6901939374a8efb3))
* **disk:** prune all unused build cache under pressure, not just &gt;24h ([718e2c8](https://github.com/emersonbusson/civm/commit/718e2c802b057959995787f3a6cae904cc035ed9))
* **orchestrator:** use df --output=avail to avoid awk escape in guest-clean ([309e24a](https://github.com/emersonbusson/civm/commit/309e24ab6343342face2906c88fc67fef6964425))


### Documentation

* **ci:** add paid-CI parity adversarial anchor docs ([a00b812](https://github.com/emersonbusson/civm/commit/a00b8123a47f4b2e573d74618cac1f56f251c33f))
* institutionalize validation.md empirical-validation log ([d69aede](https://github.com/emersonbusson/civm/commit/d69aede3a9c057cf639f603b7421d5a466b89651))
* **validation:** record orchestrator compact+START cycle proven ([aff9184](https://github.com/emersonbusson/civm/commit/aff9184e3a6aed5b0d033c75b0b32291737c4b0d))
* **vm:** bump Go to 1.26.4 in inventory ([9e8b262](https://github.com/emersonbusson/civm/commit/9e8b262ea2c2500e6e33cf73962d9f7e6de99c64))

## [1.18.7](https://github.com/emersonbusson/civm/compare/v1.18.6...v1.18.7) (2026-06-15)


### Bug Fixes

* **hook:** idle-gate cache trim so it never races a sibling build ([#133](https://github.com/emersonbusson/civm/issues/133)) ([3722f63](https://github.com/emersonbusson/civm/commit/3722f637d6562d3fef875352a89288c7f0469150))

## [1.18.6](https://github.com/emersonbusson/civm/compare/v1.18.5...v1.18.6) (2026-06-15)


### Refactor

* **civm:** dedup + decompose god-functions + safe docker reclaim ([#130](https://github.com/emersonbusson/civm/issues/130)) ([14860e5](https://github.com/emersonbusson/civm/commit/14860e5bd6549c6f3f6a8c53bc5810892fefee0e))


### Documentation

* fix doc-vs-reality contradictions across civm docs ([#131](https://github.com/emersonbusson/civm/issues/131)) ([9eaa05e](https://github.com/emersonbusson/civm/commit/9eaa05ed883e500df962fdd32b129975d7cb6dac))

## [1.18.5](https://github.com/emersonbusson/civm/compare/v1.18.4...v1.18.5) (2026-06-15)


### Bug Fixes

* **cleanup:** floor in-flight cache dirs in the emergency reclaim ([#126](https://github.com/emersonbusson/civm/issues/126)) ([12780ad](https://github.com/emersonbusson/civm/commit/12780ad0d48a6628eb1c22fc581a5ba26478ede5))


### Documentation

* **specs:** re-ground runner architecture with SPECv4 (Slice -1) ([#127](https://github.com/emersonbusson/civm/issues/127)) ([b587de6](https://github.com/emersonbusson/civm/commit/b587de65225315ce91e204c46b7fe9f7e60e2435))
* tighten redundant prose across specs, runbooks, prompts ([#128](https://github.com/emersonbusson/civm/issues/128)) ([9537e21](https://github.com/emersonbusson/civm/commit/9537e21db50cb74337bdd68f8ed1360bd9dbd8ca))

## [1.18.4](https://github.com/emersonbusson/civm/compare/v1.18.3...v1.18.4) (2026-06-15)


### Bug Fixes

* **cachetrim:** enforce MaxBytes as a hard ceiling over MinProtect ([#124](https://github.com/emersonbusson/civm/issues/124)) ([b985032](https://github.com/emersonbusson/civm/commit/b98503270d073d2c761516c10fab3e7d0cfd837c))

## [1.18.3](https://github.com/emersonbusson/civm/compare/v1.18.2...v1.18.3) (2026-06-15)


### Bug Fixes

* **reclaim:** guarantee host V: reclaim liveness (phantom + guest-prune) ([#122](https://github.com/emersonbusson/civm/issues/122)) ([27fae9c](https://github.com/emersonbusson/civm/commit/27fae9c70116e651147dec9a2a31cf1568b870df))

## [1.18.2](https://github.com/emersonbusson/civm/compare/v1.18.1...v1.18.2) (2026-06-14)


### Bug Fixes

* **ciguard:** reject bare repo-local flock as docker-heavy protection ([#119](https://github.com/emersonbusson/civm/issues/119)) ([d35b5d2](https://github.com/emersonbusson/civm/commit/d35b5d2e57dbe0b80cd8049f88ff53c08b65a08c))
* **runner:** harden the four failure layers behind the 2026-06-10 outage ([#117](https://github.com/emersonbusson/civm/issues/117)) ([16a913a](https://github.com/emersonbusson/civm/commit/16a913a873bfb6818d1518aa5c07cc1149c3dbae))

## [1.18.1](https://github.com/emersonbusson/civm/compare/v1.18.0...v1.18.1) (2026-06-07)


### Bug Fixes

* **runner:** self-cleaning runner — kill the V: disk death-spiral durably ([#115](https://github.com/emersonbusson/civm/issues/115)) ([ff5d2b9](https://github.com/emersonbusson/civm/commit/ff5d2b93fe223659eae442a11450f057dd108b18))

## [1.18.0](https://github.com/emersonbusson/civm/compare/v1.17.2...v1.18.0) (2026-06-05)


### Features

* **admit:** memory-aware admission gate (civmctl admit) ([#112](https://github.com/emersonbusson/civm/issues/112)) ([801319a](https://github.com/emersonbusson/civm/commit/801319a2df52b7237ef09fdd246e42c2bc0f4a04))

## [1.17.2](https://github.com/emersonbusson/civm/compare/v1.17.1...v1.17.2) (2026-06-05)


### Documentation

* **disciplines:** rewrite SSDV3-PROMPTS civm-native ([#110](https://github.com/emersonbusson/civm/issues/110)) ([c5e6edb](https://github.com/emersonbusson/civm/commit/c5e6edbb3b6f6fe447238a14e3a26048ae0ff2dc))

## [1.17.1](https://github.com/emersonbusson/civm/compare/v1.17.0...v1.17.1) (2026-06-05)


### Documentation

* purge discontinued compexhub peer from runbooks/specs ([#109](https://github.com/emersonbusson/civm/issues/109)) ([11e46bf](https://github.com/emersonbusson/civm/commit/11e46bf7908ca3c1192c830d7c4be6ea9eac9b73))
* **rules:** purge web-app/compexhub content, make rules civm-native ([#107](https://github.com/emersonbusson/civm/issues/107)) ([4e25e9e](https://github.com/emersonbusson/civm/commit/4e25e9efd8c495c099f0df571ef8669c67c0e46c))

## [1.17.0](https://github.com/emersonbusson/civm/compare/v1.16.0...v1.17.0) (2026-06-05)


### Features

* **reclaim:** instrument autoreclaim with scratch high-water poll ([#104](https://github.com/emersonbusson/civm/issues/104)) ([4e7e51c](https://github.com/emersonbusson/civm/commit/4e7e51cde7835877c6d16a38aac4178ab4443bbd))

## [1.16.0](https://github.com/emersonbusson/civm/compare/v1.15.0...v1.16.0) (2026-06-05)


### Features

* **reclaim:** SPECv3 admission gate breaks host headroom deadlock ([#100](https://github.com/emersonbusson/civm/issues/100)) ([25bb84c](https://github.com/emersonbusson/civm/commit/25bb84c0caebece89961f7b751e9b7bf8d8631fb))


### Bug Fixes

* **optimize:** sudo the maintenance drain (root-owned lock) ([#103](https://github.com/emersonbusson/civm/issues/103)) ([6d535be](https://github.com/emersonbusson/civm/commit/6d535be3389dd05781305cd77c853a53304520db))

## [1.15.0](https://github.com/emersonbusson/civm/compare/v1.14.2...v1.15.0) (2026-06-05)


### Features

* **memwatchdog:** monitoramento de RAM em tempo real ([d7c1b08](https://github.com/emersonbusson/civm/commit/d7c1b08653b51bf517ae3ce6c04b38188746476e))
* **memwatchdog:** real-time RAM pressure monitoring ([ae52277](https://github.com/emersonbusson/civm/commit/ae52277daa1b31f57cf1a02afc9ceece62f425c1))
* **templates:** cancel-on-pr-close workflow template ([e71bfa2](https://github.com/emersonbusson/civm/commit/e71bfa288b053d1eb9de7ddb53a0c083726b13b0))
* **templates:** cancel-on-pr-close workflow template ([0fef0c4](https://github.com/emersonbusson/civm/commit/0fef0c448ae3b84b08942db0bb0fb830561a8260))


### Bug Fixes

* **host:** vhdx watchdog honors both reclaim locks before start ([#98](https://github.com/emersonbusson/civm/issues/98)) ([4f33111](https://github.com/emersonbusson/civm/commit/4f331118c7c6ebd16738d9fce4e4dfc18f262144))


### Documentation

* **disciplines:** add architectural-noise audit superprompt ([#96](https://github.com/emersonbusson/civm/issues/96)) ([78bcf75](https://github.com/emersonbusson/civm/commit/78bcf758ff2377ff21fd7b76a825ef72237a3fc9))
* **spec:** SPECv3 host reclaim deadlock resilience ([#99](https://github.com/emersonbusson/civm/issues/99)) ([1e882d1](https://github.com/emersonbusson/civm/commit/1e882d1ffa6d5bae9134b0a5584a1c305fdec598))

## [1.14.2](https://github.com/emersonbusson/civm/compare/v1.14.1...v1.14.2) (2026-06-04)


### Bug Fixes

* **reaper:** force-cancel para drenar runs presos em queued ([9155971](https://github.com/emersonbusson/civm/commit/9155971ab422c455ff61c935dde75c1c9cb4109e))
* **reaper:** force-cancel to actually drain runs stuck queued ([193de84](https://github.com/emersonbusson/civm/commit/193de84faabb57433992715aaf97e5a3b6fa6360))


### Documentation

* **disciplines:** rewrite as civm-native, drop compexhub templates ([fbea438](https://github.com/emersonbusson/civm/commit/fbea4389c842410b34457d27d9d660b10ef49fa6))
* **rules:** add Go coding-style hygiene rule ([b7ac136](https://github.com/emersonbusson/civm/commit/b7ac136bccbf3fe7550554f8171d5fda6a721ef0))
* **rules:** regra de coding-style (higiene de código Go) ([b986b33](https://github.com/emersonbusson/civm/commit/b986b33e673ede2e2e365ee76f24f4086a8e6fbb))

## [1.14.1](https://github.com/emersonbusson/civm/compare/v1.14.0...v1.14.1) (2026-06-04)


### Documentation

* **templates:** drop redundant self-contained boilerplate ([bb2dd93](https://github.com/emersonbusson/civm/commit/bb2dd931ada80f5cadde9b4bb482a97f4bf9ff9f))
* **templates:** drop redundant self-contained boilerplate ([e89c366](https://github.com/emersonbusson/civm/commit/e89c36661a2d975236ea96b42122570c9ad37815))

## [1.14.0](https://github.com/emersonbusson/civm/compare/v1.13.1...v1.14.0) (2026-06-04)


### Features

* **reaper:** cancel queued/in_progress runs of closed PRs ([10d1d13](https://github.com/emersonbusson/civm/commit/10d1d136dc83e0cb77e3027ecd8500fde88b62c3))
* **reaper:** civmctl reap-runs — cancela runs de PRs já fechados ([e72433a](https://github.com/emersonbusson/civm/commit/e72433aa8ec86dde8c28379c56555cd858be85ca))

## [1.13.1](https://github.com/emersonbusson/civm/compare/v1.13.0...v1.13.1) (2026-06-04)


### Bug Fixes

* **host:** clamp autoreclaim gap math with [int64]0 to stop V: fill ([1b70ddd](https://github.com/emersonbusson/civm/commit/1b70ddde3b069e0d4dedd7e150e1b01e97dbfbc9))
* **host:** stop V: fill (autoreclaim Int32) + auto-restart wedged runner ([2b4ff33](https://github.com/emersonbusson/civm/commit/2b4ff334601766adae6ec2ed9c5b17dd126612f6))

## [1.13.0](https://github.com/emersonbusson/civm/compare/v1.12.0...v1.13.0) (2026-06-02)


### Features

* **runner:** auto-restart wedged runner via hooks.jsonl sentinel ([#79](https://github.com/emersonbusson/civm/issues/79)) ([ce1ad5c](https://github.com/emersonbusson/civm/commit/ce1ad5c5ad35db5adb4bdfc3d932503fc33c2b6b))

## [1.12.0](https://github.com/emersonbusson/civm/compare/v1.11.3...v1.12.0) (2026-06-02)


### Features

* **hook:** log runner WorkRoot in hook record ([#78](https://github.com/emersonbusson/civm/issues/78)) ([ec1286b](https://github.com/emersonbusson/civm/commit/ec1286b25acdf168841908da8672468dcd2c31f4))


### Bug Fixes

* **cleanup:** treat safedelete refusal as non-fatal skip ([#76](https://github.com/emersonbusson/civm/issues/76)) ([f65a5ac](https://github.com/emersonbusson/civm/commit/f65a5ac40ca1cd595b2480e25c3ec8d961eeadef))

## [1.11.3](https://github.com/emersonbusson/civm/compare/v1.11.2...v1.11.3) (2026-06-02)


### Bug Fixes

* **cleanup:** reclaim unused docker space on busy host ([#71](https://github.com/emersonbusson/civm/issues/71)) ([d28a067](https://github.com/emersonbusson/civm/commit/d28a067c0d4098a95d8f6a1b2ee4d77c476fc505))
* **cleanup:** render benign deferrals as deferido ([#73](https://github.com/emersonbusson/civm/issues/73)) ([09428fc](https://github.com/emersonbusson/civm/commit/09428fcd9a9493c7b11e5d45f4da85c1c83652ae))

## [1.11.2](https://github.com/emersonbusson/civm/compare/v1.11.1...v1.11.2) (2026-06-01)


### Bug Fixes

* **runner:** treat host-busy watchdog skip as success ([#68](https://github.com/emersonbusson/civm/issues/68)) ([f0b0760](https://github.com/emersonbusson/civm/commit/f0b0760940e5c2920511f5b598a9f21e8494be9f))

## [1.11.1](https://github.com/emersonbusson/civm/compare/v1.11.0...v1.11.1) (2026-05-31)


### Bug Fixes

* **infra:** harden host vhdx autoreclaim ([4cd6f87](https://github.com/emersonbusson/civm/commit/4cd6f87acbcb0764016c296c17f99a539b451c93))
* **infra:** harden host vhdx autoreclaim ([aa7357d](https://github.com/emersonbusson/civm/commit/aa7357dde763af0afa0eb2eed79aba2b11ade8c3))


### Documentation

* refresh SSDV3 to latest workspace model ([1fd25d3](https://github.com/emersonbusson/civm/commit/1fd25d384f1d94f1c2e4aa708b8a0f7df6cea2b1))
* refresh SSDV3 to latest workspace model ([89569f9](https://github.com/emersonbusson/civm/commit/89569f97f4bfe8950adf6c93ea11f5777c2a3bde))

## [1.11.0](https://github.com/emersonbusson/civm/compare/v1.10.0...v1.11.0) (2026-05-31)


### Features

* **runner:** harden privileged primitives + sync Kahneman discipline [#13](https://github.com/emersonbusson/civm/issues/13) ([#63](https://github.com/emersonbusson/civm/issues/63)) ([f222f4d](https://github.com/emersonbusson/civm/commit/f222f4d41f38313f23e0a09911b12b4643a7e0b9))


### Bug Fixes

* **safedelete:** escalate root-owned _work targets and gate it for real ([#61](https://github.com/emersonbusson/civm/issues/61)) ([61e4450](https://github.com/emersonbusson/civm/commit/61e44504f37d3b8a8113869362049412e8b73376))

## [1.10.0](https://github.com/emersonbusson/civm/compare/v1.9.0...v1.10.0) (2026-05-30)


### Features

* **runner:** privileged-safe cleanup of root-owned runner _work ([#59](https://github.com/emersonbusson/civm/issues/59)) ([26739ae](https://github.com/emersonbusson/civm/commit/26739aedfb4f590b33f96658cb79fd5c3af50efd))

## [1.9.0](https://github.com/emersonbusson/civm/compare/v1.8.2...v1.9.0) (2026-05-29)


### Features

* **runner:** multi-project isolation and host-volume reclamation ([#57](https://github.com/emersonbusson/civm/issues/57)) ([ddd569e](https://github.com/emersonbusson/civm/commit/ddd569e32e45500aaebdddde0c65aaceae14a5a3))

## [1.8.2](https://github.com/emersonbusson/civm/compare/v1.8.1...v1.8.2) (2026-05-28)


### Bug Fixes

* **hook:** make job-started apt/journal/fstrim cleanup best-effort ([#55](https://github.com/emersonbusson/civm/issues/55)) ([f9c9add](https://github.com/emersonbusson/civm/commit/f9c9addb2965e14105c5c6007eed2b811ed64f51))
* **hook:** make job-started cleanup safe for concurrent jobs on shared runner ([#53](https://github.com/emersonbusson/civm/issues/53)) ([6ec6b69](https://github.com/emersonbusson/civm/commit/6ec6b6986cdad4f1b5025083dfd6eef2038ecdea))
* **hook:** prune dangling images only, never -a, on shared runner ([#56](https://github.com/emersonbusson/civm/issues/56)) ([36d26b1](https://github.com/emersonbusson/civm/commit/36d26b1cfc9875cc1bfe1035ab2e6dd528b13ab2))

## [1.8.1](https://github.com/emersonbusson/civm/compare/v1.8.0...v1.8.1) (2026-05-25)


### Bug Fixes

* **templates:** limit Acme router vet to local modules ([#51](https://github.com/emersonbusson/civm/issues/51)) ([c454dd3](https://github.com/emersonbusson/civm/commit/c454dd36389c2b10dd6667b17e1054fc0942c5a8))

## [1.8.0](https://github.com/emersonbusson/civm/compare/v1.7.0...v1.8.0) (2026-05-23)


### Features

* **actions-metrics:** aggregate billable minutes + run counts cross-repo ([#49](https://github.com/emersonbusson/civm/issues/49)) ([e39001f](https://github.com/emersonbusson/civm/commit/e39001f2c6a460e579575cecce6ed7556c78509f))

## [1.7.0](https://github.com/emersonbusson/civm/compare/v1.6.0...v1.7.0) (2026-05-23)


### Features

* **active-runs:** aggregate in_progress + queued runs cross-repo with ETA ([#47](https://github.com/emersonbusson/civm/issues/47)) ([86e8dd4](https://github.com/emersonbusson/civm/commit/86e8dd43c782bbdc5b74044d2076b5987db1ba74))

## [1.6.0](https://github.com/emersonbusson/civm/compare/v1.5.1...v1.6.0) (2026-05-21)


### Features

* **hook:** graduated cache cleanup with per-cache caps + warn-tolerant routine ([#45](https://github.com/emersonbusson/civm/issues/45)) ([cb8f3be](https://github.com/emersonbusson/civm/commit/cb8f3be1d593716124dbfded997a0d258704d71d))

## [1.5.1](https://github.com/emersonbusson/civm/compare/v1.5.0...v1.5.1) (2026-05-21)


### Bug Fixes

* parse cleanup journal RFC3339 offsets ([70a8e96](https://github.com/emersonbusson/civm/commit/70a8e96bd7be81124ebecc62ae949d506e07007d))

## [1.5.0](https://github.com/emersonbusson/civm/compare/v1.4.0...v1.5.0) (2026-05-21)


### Features

* generalize CI hooks and doctor checks ([4d47cf6](https://github.com/emersonbusson/civm/commit/4d47cf6a2e9542524c4b8319de67a316b9070f17))
* harden runner and disk monitoring ([#41](https://github.com/emersonbusson/civm/issues/41)) ([726d002](https://github.com/emersonbusson/civm/commit/726d002481f9d167fdc561c721fa9b7b5a7755ce))


### Bug Fixes

* generate runner hook scripts ([fd87290](https://github.com/emersonbusson/civm/commit/fd8729044336a8a1a6cc87d015b549ee184e3c2d))
* load runner watchdog gh auth env ([fd644ff](https://github.com/emersonbusson/civm/commit/fd644ff2e8f4b43b8294d9f90f850167d22d2ddb))
* use app token for release automation ([e8fdcec](https://github.com/emersonbusson/civm/commit/e8fdceca67d1e485fc068bbb84b466051cfdfb65))
* use valid runner hook script paths ([8857c20](https://github.com/emersonbusson/civm/commit/8857c2004e105bd3ca210f00b9c6a7dcd073a9ce))

## [1.4.0](https://github.com/emersonbusson/civm/compare/v1.3.0...v1.4.0) (2026-05-17)


### Features

* **civm:** consolidar status operacional dos peers ([#38](https://github.com/emersonbusson/civm/issues/38)) ([f4b8ba3](https://github.com/emersonbusson/civm/commit/f4b8ba32542e77249642d51e931ef5c7819da396))
* **civmctl:** add self-upgrade subcommand ([#33](https://github.com/emersonbusson/civm/issues/33)) ([33fcbff](https://github.com/emersonbusson/civm/commit/33fcbffb260c376a516c5bfbdda0dae8509fb5a1))


### Documentation

* **runner:** formalize self-upgrade deploy workflow ([#34](https://github.com/emersonbusson/civm/issues/34)) ([c4083e8](https://github.com/emersonbusson/civm/commit/c4083e8890615445a2dae6452c1b1dff000a6d0d))
* **security:** add SECURITY.md with threat model ([#36](https://github.com/emersonbusson/civm/issues/36)) ([bc1d537](https://github.com/emersonbusson/civm/commit/bc1d53756e1fe6e8943f30dc2c0a1400b7024661))

## [1.3.0](https://github.com/emersonbusson/civm/compare/v1.2.0...v1.3.0) (2026-05-16)


### Features

* **metrics:** emit prometheus textfile for node_exporter ([#29](https://github.com/emersonbusson/civm/issues/29)) ([d24611b](https://github.com/emersonbusson/civm/commit/d24611b4db8264ad49a8d7121f66d1ac8504a811))


### Bug Fixes

* **hook:** preserve hot caches under \$HOME on job-completed ([#31](https://github.com/emersonbusson/civm/issues/31)) ([de96cbf](https://github.com/emersonbusson/civm/commit/de96cbf39a7de1159984ce55baa55fb950a97131))


### Refactor

* **hook:** use slog.JSONHandler for hook event log ([#28](https://github.com/emersonbusson/civm/issues/28)) ([8246ab7](https://github.com/emersonbusson/civm/commit/8246ab7717c43acdcc05a31c1783eb067a65aae0))

## [1.2.0](https://github.com/emersonbusson/civm/compare/v1.1.3...v1.2.0) (2026-05-16)


### Features

* **ci:** add kahneman-sync audit across 14 peer repos ([e97476f](https://github.com/emersonbusson/civm/commit/e97476ff045f0abd477068eee93aa63d4f685bf1))


### Refactor

* **hook:** eliminate shell wrappers via argv[0] dispatch ([#26](https://github.com/emersonbusson/civm/issues/26)) ([1b8ecbe](https://github.com/emersonbusson/civm/commit/1b8ecbefefb7715f70a108a87f9e5e689d2b9cd1))


### Documentation

* **governance:** align ssdv3 refs to disciplines/ path ([4375b7f](https://github.com/emersonbusson/civm/commit/4375b7f3e7de2d3cb4354980b150deb188ae18a5))


### CI

* paid approval workflow + job hooks + capacity reporting ([#25](https://github.com/emersonbusson/civm/issues/25)) ([0ace9e5](https://github.com/emersonbusson/civm/commit/0ace9e52ac510a3f4926373129cdfe1494ebbaa9))

## [1.1.3](https://github.com/emersonbusson/civm/compare/v1.1.2...v1.1.3) (2026-05-11)


### Bug Fixes

* preserve release PR component parsing ([#17](https://github.com/emersonbusson/civm/issues/17)) ([5592580](https://github.com/emersonbusson/civm/commit/55925806a4b56ab52ccb77861e88f956f349f7be))
* remove release package component ([#18](https://github.com/emersonbusson/civm/issues/18)) ([1602578](https://github.com/emersonbusson/civm/commit/1602578e06b0b450b2a94bf6f5f7b44620655256))

## [1.1.2](https://github.com/emersonbusson/civm/compare/v1.1.1...v1.1.2) (2026-05-11)


### Bug Fixes

* set grouped release PR title ([#15](https://github.com/emersonbusson/civm/issues/15)) ([1e0cfb0](https://github.com/emersonbusson/civm/commit/1e0cfb0d340d9819186b3ba54d999012348e9377))

## [1.1.1](https://github.com/emersonbusson/civm/compare/v1.1.0...v1.1.1) (2026-05-11)


### Bug Fixes

* address release-please follow-ups ([#11](https://github.com/emersonbusson/civm/issues/11)) ([25544c6](https://github.com/emersonbusson/civm/commit/25544c65675a7834b8a7acdd9a8b4e4667c67e05))

## [1.1.0](https://github.com/emersonbusson/civm/compare/v1.0.0...v1.1.0) (2026-05-11)


### Features

* add release-please automation ([#8](https://github.com/emersonbusson/civm/issues/8)) ([a93789f](https://github.com/emersonbusson/civm/commit/a93789f3d6ad5789494703db31b79d145cf575df))


### Bug Fixes

* harden civmctl bootstrap and runner operations ([#6](https://github.com/emersonbusson/civm/issues/6)) ([4a1f590](https://github.com/emersonbusson/civm/commit/4a1f590ed832990d48901665beeadc9287c8acb1))
