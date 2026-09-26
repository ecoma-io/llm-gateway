# Changelog

## [0.3.0](https://github.com/ecoma-io/llm-gateway/compare/v0.2.0...v0.3.0) (2026-09-26)


### Features

* **console-api:** bind created keys to their account in the engine ([#73](https://github.com/ecoma-io/llm-gateway/issues/73)) ([7c7c23b](https://github.com/ecoma-io/llm-gateway/commit/7c7c23be6cbdef0163ce174d313bf43a56a7591b))
* **console-api:** establish accounting and funding foundation ([#79](https://github.com/ecoma-io/llm-gateway/issues/79)) ([f5343bd](https://github.com/ecoma-io/llm-gateway/commit/f5343bd9fd81f77d3d6f6a2f047fced4ec82b1e0))
* **console-api:** establish commerce and entitlement foundation ([#59](https://github.com/ecoma-io/llm-gateway/issues/59)) ([aaf6816](https://github.com/ecoma-io/llm-gateway/commit/aaf68164d8a57ca84972b4444b48609c1a0dbccf))
* **console-api:** establish identity and API-key foundation ([#46](https://github.com/ecoma-io/llm-gateway/issues/46)) ([3ae9089](https://github.com/ecoma-io/llm-gateway/commit/3ae90894572761a69e44f1c6b61407de0305b107))
* **console-api:** establish usage fact ingestion and settlement ([#112](https://github.com/ecoma-io/llm-gateway/issues/112)) ([1127942](https://github.com/ecoma-io/llm-gateway/commit/11279421fb191717912c04def22785df48e15544))
* **console-api:** gate readiness on the store answering ([#74](https://github.com/ecoma-io/llm-gateway/issues/74)) ([21cee52](https://github.com/ecoma-io/llm-gateway/commit/21cee5246a6408012986daf66adcf129b18ef421))
* **dataplane:** establish model catalog foundation ([#57](https://github.com/ecoma-io/llm-gateway/issues/57)) ([db7d2e4](https://github.com/ecoma-io/llm-gateway/commit/db7d2e4b45b16b83dd3ffa22480cb26a154830be))
* **dataplane:** establish provider adapters and egress foundation ([#93](https://github.com/ecoma-io/llm-gateway/issues/93)) ([3c5addb](https://github.com/ecoma-io/llm-gateway/commit/3c5addb4df186341d6ac37e95e5c8dcc09cd934f))
* **dataplane:** establish runtime admission ([#68](https://github.com/ecoma-io/llm-gateway/issues/68)) ([0efc493](https://github.com/ecoma-io/llm-gateway/commit/0efc49320f116ba161d8a099e569b7968b69d6ad))
* **dataplane:** establish runtime routing foundation ([#91](https://github.com/ecoma-io/llm-gateway/issues/91)) ([4f1794e](https://github.com/ecoma-io/llm-gateway/commit/4f1794ea5b0931f74d9692e59e49b7a6f4a3c9f3))
* **dataplane:** establish runtime storage foundation ([#55](https://github.com/ecoma-io/llm-gateway/issues/55)) ([d6d9df3](https://github.com/ecoma-io/llm-gateway/commit/d6d9df302b788e5fd62745dc6543fc8c29b83a34))
* **dataplane:** establish usage close and finalization ([#97](https://github.com/ecoma-io/llm-gateway/issues/97)) ([2a0020a](https://github.com/ecoma-io/llm-gateway/commit/2a0020af9454c72f0ca97f16b05aa41f10ce4e2a))
* **openapi:** split the API contract into three documents ([#37](https://github.com/ecoma-io/llm-gateway/issues/37)) ([d0241e8](https://github.com/ecoma-io/llm-gateway/commit/d0241e8fad5fa798c4cd7ef18a9e07f8b0d1cdbd))
* **workspace:** establish control-to-data projection ([#60](https://github.com/ecoma-io/llm-gateway/issues/60)) ([ae07320](https://github.com/ecoma-io/llm-gateway/commit/ae07320c2330927c70bccf5d0eae597ff7523629))
* **workspace:** establish persistence and migration foundation ([#47](https://github.com/ecoma-io/llm-gateway/issues/47)) ([dbfefe5](https://github.com/ecoma-io/llm-gateway/commit/dbfefe56d4b570e5c907a3b4ace47399e7cd372c))
* **workspace:** harden the cross-plane architecture ([#40](https://github.com/ecoma-io/llm-gateway/issues/40)) ([02e9072](https://github.com/ecoma-io/llm-gateway/commit/02e9072923eb39e21d2c50c5b38cbdbfbb751a85))


### Bug Fixes

* **api:** scope storage transactions to the pool that opened them ([#30](https://github.com/ecoma-io/llm-gateway/issues/30)) ([c00fd73](https://github.com/ecoma-io/llm-gateway/commit/c00fd736fdab76d071fbee8f88d23ef9b32d1168)), closes [#29](https://github.com/ecoma-io/llm-gateway/issues/29)
* **console-api:** harden prerequisites before usage settlement ([#108](https://github.com/ecoma-io/llm-gateway/issues/108)) ([d19d875](https://github.com/ecoma-io/llm-gateway/commit/d19d875f3b26b4c373d90e93ae6bb365deb46fd1))
* **console-api:** verify the recorded key answers the presented id ([#71](https://github.com/ecoma-io/llm-gateway/issues/71)) ([62e345b](https://github.com/ecoma-io/llm-gateway/commit/62e345b0688cabbdc93b5d90645c0a3bddb9a2b6))
* **dataplane:** express the reaper's expired close on the persistence port ([#88](https://github.com/ecoma-io/llm-gateway/issues/88)) ([a290690](https://github.com/ecoma-io/llm-gateway/commit/a2906905bd554d35e54e8685bb6499c6ef575318))
* **dataplane:** give the reaper's tests a database of their own ([3c18790](https://github.com/ecoma-io/llm-gateway/commit/3c18790c2e0f95eaa84950ff5d4ac181f058ff28))
* **dataplane:** harden admission prerequisites ([#67](https://github.com/ecoma-io/llm-gateway/issues/67)) ([f4e0296](https://github.com/ecoma-io/llm-gateway/commit/f4e02966e0459f82abfbb0829bbc185e32d43265))
* **dataplane:** harden routing prerequisites ([#87](https://github.com/ecoma-io/llm-gateway/issues/87)) ([926cb4c](https://github.com/ecoma-io/llm-gateway/commit/926cb4ce2620705760f428a76bdd3e57745eebb2))
* **dataplane:** harden usage-close prerequisites ([#94](https://github.com/ecoma-io/llm-gateway/issues/94)) ([62915fb](https://github.com/ecoma-io/llm-gateway/commit/62915fbb0b0f2b56776cfbc46cb0d54718bbdbec))
* **dataplane:** make the reaper integration test hermetic against earlier runs ([#82](https://github.com/ecoma-io/llm-gateway/issues/82)) ([3c18790](https://github.com/ecoma-io/llm-gateway/commit/3c18790c2e0f95eaa84950ff5d4ac181f058ff28))
* **dataplane:** normalise a literal null payload to the empty object ([#69](https://github.com/ecoma-io/llm-gateway/issues/69)) ([82eb288](https://github.com/ecoma-io/llm-gateway/commit/82eb2889c14fe91ae7928e005577fa068692213c))
* **dataplane:** reject malformed usage-fact pages before cursor advance ([#42](https://github.com/ecoma-io/llm-gateway/issues/42)) ([bc4767e](https://github.com/ecoma-io/llm-gateway/commit/bc4767e237901702a148002072b5e98674d7d474))
* **dataplane:** reject usage-events rewrites in the engine ([#83](https://github.com/ecoma-io/llm-gateway/issues/83)) ([1bd04b7](https://github.com/ecoma-io/llm-gateway/commit/1bd04b78c2ee0067f9e38a0e8e2fda688dc3fea9))
* **workspace:** harden foundations before accounting ([#62](https://github.com/ecoma-io/llm-gateway/issues/62)) ([728a1af](https://github.com/ecoma-io/llm-gateway/commit/728a1aff35c8f665965b3240587f27cfc80f58f4))
* **workspace:** harden foundations before model catalog ([#56](https://github.com/ecoma-io/llm-gateway/issues/56)) ([ee12c39](https://github.com/ecoma-io/llm-gateway/commit/ee12c393083c6266312f4414490f336b931dfde5))
* **workspace:** keep the golangci-lint cache out of sibling worktrees ([#76](https://github.com/ecoma-io/llm-gateway/issues/76)) ([663ad18](https://github.com/ecoma-io/llm-gateway/commit/663ad181ef8ed68cd6681804ef82c87881538246))


### Documentation

* **api:** ground the linter-roster comment in the current go.mod ([#33](https://github.com/ecoma-io/llm-gateway/issues/33)) ([0035497](https://github.com/ecoma-io/llm-gateway/commit/0035497f6686a6cbf8874619ea78ae03c0ed8197)), closes [#28](https://github.com/ecoma-io/llm-gateway/issues/28)
* **console-api:** record the console sign-in identity decision ([#81](https://github.com/ecoma-io/llm-gateway/issues/81)) ([0b6eec4](https://github.com/ecoma-io/llm-gateway/commit/0b6eec4f72d20ab89246a5eefc45e27555a41d7c))
* place funding buckets in the accounting context ([#31](https://github.com/ecoma-io/llm-gateway/issues/31)) ([32d11ce](https://github.com/ecoma-io/llm-gateway/commit/32d11cea87d5dd35d3987a262be24bfb9dd33701)), closes [#26](https://github.com/ecoma-io/llm-gateway/issues/26)
* reconcile the ledger's hold accounting with the usage-facts feed ([#84](https://github.com/ecoma-io/llm-gateway/issues/84)) ([5829915](https://github.com/ecoma-io/llm-gateway/commit/582991578be89c74d84c0a8f38280366823b2c3d))
* state the AI disclosure trailer rule once ([#32](https://github.com/ecoma-io/llm-gateway/issues/32)) ([0d2c673](https://github.com/ecoma-io/llm-gateway/commit/0d2c673a7497a475d50c6f011a9c61a2a8819001)), closes [#27](https://github.com/ecoma-io/llm-gateway/issues/27)
* **workspace:** move the documentation onto the two planes ([#38](https://github.com/ecoma-io/llm-gateway/issues/38)) ([026822d](https://github.com/ecoma-io/llm-gateway/commit/026822dfd4c9aef1dc6f55d624de973397517dbb))
* **workspace:** restate the lint flag's policy after the cache pin ([#78](https://github.com/ecoma-io/llm-gateway/issues/78)) ([16917d3](https://github.com/ecoma-io/llm-gateway/commit/16917d364f1718b9dfe9f3dee2ecc9358edf6d47))

## [0.2.0](https://github.com/ecoma-io/llm-gateway/compare/v0.1.0...v0.2.0) (2026-09-23)


### Features

* **api:** add a redis-compatible infrastructure foundation ([#12](https://github.com/ecoma-io/llm-gateway/issues/12)) ([084bfc2](https://github.com/ecoma-io/llm-gateway/commit/084bfc271e886eb96b2cb2b43aad632abfd0aa0e))
* **api:** establish the first application boundary ([#11](https://github.com/ecoma-io/llm-gateway/issues/11)) ([bf3688a](https://github.com/ecoma-io/llm-gateway/commit/bf3688a36fa57d9ff0ff77929d3d549cbfb9c6ba))
* **api:** establish the timescaledb storage and migration foundation ([#10](https://github.com/ecoma-io/llm-gateway/issues/10)) ([25e5d2a](https://github.com/ecoma-io/llm-gateway/commit/25e5d2a2c1811ba4fa52a161948434d91f6202ce)), closes [#8](https://github.com/ecoma-io/llm-gateway/issues/8)
* **web:** adopt loom and build the gateway app shell ([#16](https://github.com/ecoma-io/llm-gateway/issues/16)) ([06e5f2f](https://github.com/ecoma-io/llm-gateway/commit/06e5f2f810107d13a5843e985d09cd4ebf6d845a)), closes [#6](https://github.com/ecoma-io/llm-gateway/issues/6)


### Bug Fixes

* **api:** expose one Redis connection I/O timeout ([#23](https://github.com/ecoma-io/llm-gateway/issues/23)) ([be4db64](https://github.com/ecoma-io/llm-gateway/commit/be4db64a7aeb34d256ea98a322c6d03b95da4fcf))


### Documentation

* align repository map with landed foundation ([#25](https://github.com/ecoma-io/llm-gateway/issues/25)) ([7931eb4](https://github.com/ecoma-io/llm-gateway/commit/7931eb4b3feccea95e0e4455bcb58a25290b7421))
* define the gateway domain model as ADRs ([#15](https://github.com/ecoma-io/llm-gateway/issues/15)) ([009e567](https://github.com/ecoma-io/llm-gateway/commit/009e567fa0215f62d4db92b5719efb6663a9ff38))
* distinguish aggregate boundaries from coordinated transactions ([#24](https://github.com/ecoma-io/llm-gateway/issues/24)) ([7422b1c](https://github.com/ecoma-io/llm-gateway/commit/7422b1cc40421b062fbf3a3dc520304fda74f187))

## 0.1.0 (2026-09-22)


### Documentation

* note that main is governed and how a change lands ([#1](https://github.com/ecoma-io/llm-gateway/issues/1)) ([b760bcd](https://github.com/ecoma-io/llm-gateway/commit/b760bcdf7fa72cb60171bda8dbf5f5e6d3953841))
