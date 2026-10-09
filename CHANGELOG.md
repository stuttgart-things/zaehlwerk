# Changelog

## [0.12.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.11.0...v0.12.0) (2026-10-09)


### Features

* a display variant in the table mock, two zones instead of gestures ([#73](https://github.com/stuttgart-things/zaehlwerk/issues/73)) ([54a953f](https://github.com/stuttgart-things/zaehlwerk/commit/54a953f34d60ab94eb18e41fdc1e25e86f4c2b54)), closes [#72](https://github.com/stuttgart-things/zaehlwerk/issues/72)

## [0.11.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.10.0...v0.11.0) (2026-10-09)


### Features

* run the piezo board from the buttons page, one table mock ([#70](https://github.com/stuttgart-things/zaehlwerk/issues/70)) ([6aae010](https://github.com/stuttgart-things/zaehlwerk/commit/6aae010d9d088f9e5ecb7faf6b0cf798289e1d0c))

## [0.10.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.9.0...v0.10.0) (2026-10-09)


### Features

* a mock of the table's buttons and their hub, pressed on a page ([#67](https://github.com/stuttgart-things/zaehlwerk/issues/67)) ([b02553f](https://github.com/stuttgart-things/zaehlwerk/commit/b02553fb6ec819d33d85a0db097a32eeece42d25))


### Bug Fixes

* **deps:** update module github.com/redis/go-redis/v9 to v9.23.0 ([#65](https://github.com/stuttgart-things/zaehlwerk/issues/65)) ([32b974f](https://github.com/stuttgart-things/zaehlwerk/commit/32b974fce5893f1e649150c4484a0227d0fa75b8))

## [0.9.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.8.0...v0.9.0) (2026-10-02)


### Features

* GET /live/stream follows the table, not one match, with player ids when reported ([#62](https://github.com/stuttgart-things/zaehlwerk/issues/62)) ([d5f7c0e](https://github.com/stuttgart-things/zaehlwerk/commit/d5f7c0e7cb04c970bcd773c971a0bd812c8f7437)), closes [#48](https://github.com/stuttgart-things/zaehlwerk/issues/48)

## [0.8.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.7.0...v0.8.0) (2026-09-30)


### Features

* **ci:** per-PR kustomize base, cleanup and preview URL for PR previews ([#54](https://github.com/stuttgart-things/zaehlwerk/issues/54)) ([c08503e](https://github.com/stuttgart-things/zaehlwerk/commit/c08503e7934fdf8ca1437b009b48694303561de9))
* let a board find the running match before its first point ([#57](https://github.com/stuttgart-things/zaehlwerk/issues/57)) ([82855e8](https://github.com/stuttgart-things/zaehlwerk/commit/82855e873fdc6b34738835f3047ca1f3ef33d3c9)), closes [#53](https://github.com/stuttgart-things/zaehlwerk/issues/53)
* mock the chain piezo → zaehlwerk → Schmetterpause ([#58](https://github.com/stuttgart-things/zaehlwerk/issues/58)) ([27f9ab1](https://github.com/stuttgart-things/zaehlwerk/commit/27f9ab16fd6eb2a50ca4a59473ac1b6da94afb36))

## [0.7.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.6.0...v0.7.0) (2026-09-26)


### Features

* offer a single game on the scoring page (best of 1) ([#51](https://github.com/stuttgart-things/zaehlwerk/issues/51)) ([3f89817](https://github.com/stuttgart-things/zaehlwerk/commit/3f8981729348120877bd1c43a2a428a43e6edbfa))

## [0.6.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.5.0...v0.6.0) (2026-09-25)


### Features

* offer observers to keep score, from Schmetterpause's /api/operators ([#49](https://github.com/stuttgart-things/zaehlwerk/issues/49)) ([1f33431](https://github.com/stuttgart-things/zaehlwerk/commit/1f33431f808e2c2717738717af8d384e667bcebf))

## [0.5.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.4.1...v0.5.0) (2026-09-16)


### Features

* trust the cluster CA, so schmetterpause is reachable over its HTTPRoute ([#45](https://github.com/stuttgart-things/zaehlwerk/issues/45)) ([e1be604](https://github.com/stuttgart-things/zaehlwerk/commit/e1be604037ecc035ed139dadbf3f1338de2e0a77))

## [0.4.1](https://github.com/stuttgart-things/zaehlwerk/compare/v0.4.0...v0.4.1) (2026-09-16)


### Bug Fixes

* **deps:** update module github.com/stuttgart-things/homerun-library/v4 to v4.5.0 ([#39](https://github.com/stuttgart-things/zaehlwerk/issues/39)) ([4d2f3c0](https://github.com/stuttgart-things/zaehlwerk/commit/4d2f3c091650b12f49f047116e35990ae2486b24))
* let SCHMETTERPAUSE_TOKEN reach the container ([#43](https://github.com/stuttgart-things/zaehlwerk/issues/43)) ([4087184](https://github.com/stuttgart-things/zaehlwerk/commit/4087184e9580188aea902ce2b408feff0d27c9a7))

## [0.4.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.3.0...v0.4.0) (2026-09-16)


### Features

* report a finished match to schmetterpause ([#41](https://github.com/stuttgart-things/zaehlwerk/issues/41)) ([1c3f6de](https://github.com/stuttgart-things/zaehlwerk/commit/1c3f6def8f601cf9dbde87d590b24b4925010a11))

## [0.3.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.2.0...v0.3.0) (2026-09-11)


### Features

* give the scoring page schmetterpause's look and lay it out for a phone ([#38](https://github.com/stuttgart-things/zaehlwerk/issues/38)) ([2ad94ea](https://github.com/stuttgart-things/zaehlwerk/commit/2ad94ea922c7d2eae560bda4ea865f2d08fcb7bc))
* tag each pitched transition with its kind and the side it went to ([#36](https://github.com/stuttgart-things/zaehlwerk/issues/36)) ([e040082](https://github.com/stuttgart-things/zaehlwerk/commit/e040082f7f6716c985f99141645df97dd93fbb6d)), closes [#35](https://github.com/stuttgart-things/zaehlwerk/issues/35)

## [0.2.0](https://github.com/stuttgart-things/zaehlwerk/compare/v0.1.0...v0.2.0) (2026-09-08)


### Features

* a KCL module for running this on Kubernetes ([#31](https://github.com/stuttgart-things/zaehlwerk/issues/31)) ([acc45ff](https://github.com/stuttgart-things/zaehlwerk/commit/acc45ffb3046ac711e1a1e355ba5a83cc30a08c9))

## 0.1.0 (2026-09-08)


### Features

* hold the score on the panel instead of timing it out ([#12](https://github.com/stuttgart-things/zaehlwerk/issues/12)) ([9e2943f](https://github.com/stuttgart-things/zaehlwerk/commit/9e2943f5ce7bd799125a95eb6dfafe6096ae94de))
* homerun pitcher sink on the tabletennis stream ([#11](https://github.com/stuttgart-things/zaehlwerk/issues/11)) ([6ea9d7d](https://github.com/stuttgart-things/zaehlwerk/commit/6ea9d7d136e666774ab815102c58a54a7c5883ad)), closes [#2](https://github.com/stuttgart-things/zaehlwerk/issues/2)
* ingest endpoints for button, piezo and web sources ([#9](https://github.com/stuttgart-things/zaehlwerk/issues/9)) ([6911412](https://github.com/stuttgart-things/zaehlwerk/commit/69114122f1088af719807599cd722eb70937fa4b)), closes [#3](https://github.com/stuttgart-things/zaehlwerk/issues/3)
* match scorer with set logic and idempotent event handling ([#6](https://github.com/stuttgart-things/zaehlwerk/issues/6)) ([8aaf6f6](https://github.com/stuttgart-things/zaehlwerk/commit/8aaf6f6e8a59c999001207c33ce034febf281f59)), closes [#1](https://github.com/stuttgart-things/zaehlwerk/issues/1)
* pitch through homerun2-omni-pitcher over HTTP ([#17](https://github.com/stuttgart-things/zaehlwerk/issues/17)) ([6f1bfb7](https://github.com/stuttgart-things/zaehlwerk/commit/6f1bfb77d819f1792926216a255667d37e465025))
* score a match from the browser, htmx and all ([#18](https://github.com/stuttgart-things/zaehlwerk/issues/18)) ([7afe50e](https://github.com/stuttgart-things/zaehlwerk/commit/7afe50e2ca0292bef93c12d6005e57148e7324c8))
* SSE endpoint for the live score ([#13](https://github.com/stuttgart-things/zaehlwerk/issues/13)) ([b0c2bc1](https://github.com/stuttgart-things/zaehlwerk/commit/b0c2bc11678573cca2dc9e06993342dce44db8b2)), closes [#4](https://github.com/stuttgart-things/zaehlwerk/issues/4)
* switch the led-catcher stream for the duration of a match ([#15](https://github.com/stuttgart-things/zaehlwerk/issues/15)) ([88a9511](https://github.com/stuttgart-things/zaehlwerk/commit/88a9511376c08cc609e5b3d7a4569fb8e27b2e38)), closes [#5](https://github.com/stuttgart-things/zaehlwerk/issues/5)
* version the binary and cut releases with release-please ([#27](https://github.com/stuttgart-things/zaehlwerk/issues/27)) ([b0edcab](https://github.com/stuttgart-things/zaehlwerk/commit/b0edcab73fe869812e8be437df977efee7049a6f)), closes [#22](https://github.com/stuttgart-things/zaehlwerk/issues/22)


### Bug Fixes

* take the build stamp from the shared workflow's own variables ([#29](https://github.com/stuttgart-things/zaehlwerk/issues/29)) ([32d8aef](https://github.com/stuttgart-things/zaehlwerk/commit/32d8aef0ca896f0073efe5252658c620f6c33c9c)), closes [#22](https://github.com/stuttgart-things/zaehlwerk/issues/22)
