# Changelog

## [0.5.0](https://github.com/VarroaCI/varroa-jenkins/compare/v0.4.1...v0.5.0) (2026-10-18)


### Features

* **api:** report the Jenkins upstream state on version candidates ([5c54e55](https://github.com/VarroaCI/varroa-jenkins/commit/5c54e5580831e6e241d8037973579e921b936e0d))
* **chart:** expose externalTrafficPolicy on the member endpoint ([4495787](https://github.com/VarroaCI/varroa-jenkins/commit/4495787eafb1289311056d6aa21f0d44cdc8ede4))
* **chart:** set the external NATS traffic policy ([4e9cbc5](https://github.com/VarroaCI/varroa-jenkins/commit/4e9cbc5944f42a1cf4b69a83369f38cd40f2bf6d))
* **checkoutbound:** scan frontend source for outbound hosts ([95fb851](https://github.com/VarroaCI/varroa-jenkins/commit/95fb851c0745becadf0180a515d65e25d4db6c78))
* **controller:** set the runtime update site to the in-cluster update center ([53ddbde](https://github.com/VarroaCI/varroa-jenkins/commit/53ddbde8c0a7304f106a714a0b44a8b1836c4c65))
* **hack:** add outbound connection inventory check ([408153c](https://github.com/VarroaCI/varroa-jenkins/commit/408153c3b078398ebf264349d1a9be2c171e395f))
* **operator:** add offline switches for Jenkins upstream reads and images ([cf870a3](https://github.com/VarroaCI/varroa-jenkins/commit/cf870a3cb87bf90942c4712c51afa66e6333dc01))
* **varroactl:** add the air-gap release transfer bundle ([5a0d41a](https://github.com/VarroaCI/varroa-jenkins/commit/5a0d41af4c0154759c4405109106ab1b746153d4))


### Bug Fixes

* **bundle:** peel annotated tags to their commit SHA ([bbb680c](https://github.com/VarroaCI/varroa-jenkins/commit/bbb680c5a76634d808c87a5270026fccebb2ab5b))
* **chart:** fail closed for egress groups that name an address ([a615a3b](https://github.com/VarroaCI/varroa-jenkins/commit/a615a3bc8ea25cb9270b96721939d0525fb23f8f))
* **chart:** pin the remaining egress groups ([b2de0eb](https://github.com/VarroaCI/varroa-jenkins/commit/b2de0ebf35bba84208fca6774f07f8773f1d9b48))
* **controller:** roll Connected controllers on a Jenkins upstream change ([f2b80b7](https://github.com/VarroaCI/varroa-jenkins/commit/f2b80b7bf581969892514a3e2cd59342fa63a486))
* **mite:** keep the tenant grace clock across a reconnect ([1e27fb5](https://github.com/VarroaCI/varroa-jenkins/commit/1e27fb589db6f586cf920ea8120c0a49ec510803))
* **operator:** open the mite inbound subscription from Provisioning ([5cbe37a](https://github.com/VarroaCI/varroa-jenkins/commit/5cbe37af3ede10db699cf2ad5460b80b66a7365e))
* **operator:** raise the upgrade tracker stable/ read cap ([9ffd572](https://github.com/VarroaCI/varroa-jenkins/commit/9ffd5724060d85a243712387a4210f21022b18f8))
* **varroactl:** check the git host for export catalog secrets ([2ed3ae6](https://github.com/VarroaCI/varroa-jenkins/commit/2ed3ae6a425cdcd168ee2ecf02ccb454d74bac9c))
* **varroactl:** export a directory-convention catalog ([465a1c3](https://github.com/VarroaCI/varroa-jenkins/commit/465a1c3ab5e4ff9601d8e9096e5d1b86c2adb0c0))
* **varroactl:** pack only the --path directory in export bundle ([da556b7](https://github.com/VarroaCI/varroa-jenkins/commit/da556b7c4e99146550cf2180922bfd52bffcbfb0))
* **varroactl:** refuse uc:// for export bundle and catalog ([8c59b36](https://github.com/VarroaCI/varroa-jenkins/commit/8c59b36828a809031c7537d66c9a7dd01a9a3162))

## [0.4.1](https://github.com/VarroaCI/varroa-jenkins/compare/v0.4.0...v0.4.1) (2026-10-16)


### Bug Fixes

* **catalog:** invalidate a derived item whose closure is incomplete ([b562e6b](https://github.com/VarroaCI/varroa-jenkins/commit/b562e6bcdc378661147d4e224a5c785c69479af3))
* **cells:** cap projected tenant credentials per cell ([87e47eb](https://github.com/VarroaCI/varroa-jenkins/commit/87e47eb089486a2817d31f836c99effbe33d48fd))
* **controller:** check every member cell for a tenant hold ([948bcdc](https://github.com/VarroaCI/varroa-jenkins/commit/948bcdce23065ba680b6818bbd934f371f1a8a93))
* **controller:** fold tenant warnings from every member cell ([abc766e](https://github.com/VarroaCI/varroa-jenkins/commit/abc766e96e60091c37de2a816015664eb3c8b521))
* **controller:** mark a rehydration give-up with a flag ([3184cc3](https://github.com/VarroaCI/varroa-jenkins/commit/3184cc36402af104a7db67a2d08c9fcec39e6830))
* **frontend:** disable Add to bundle for an invalid catalog item ([16034fc](https://github.com/VarroaCI/varroa-jenkins/commit/16034fc6c6cfb553b884a59e94e79b09e6d14346))
* **mite:** answer BUSY when a restart-copy envelope publish fails ([c3b66d5](https://github.com/VarroaCI/varroa-jenkins/commit/c3b66d521c4285af432a3e251c391034c1933681))
* **mite:** answer the unresolved-tenant grace as tenant-unresolved ([3f1cc9f](https://github.com/VarroaCI/varroa-jenkins/commit/3f1cc9fadbc6ec3873e7b006a6838ed347cbd945))
* **mite:** cap the claim wait under the loopback write timeout ([35bcec0](https://github.com/VarroaCI/varroa-jenkins/commit/35bcec06534cd1c3e0008cbc6485a601dc6382d1))
* **mite:** cap the cross-tenant wait one second inside the write timeout ([bde1923](https://github.com/VarroaCI/varroa-jenkins/commit/bde192313a40245a4153ba224f1640b8acaf1303))
* **mite:** carry a plain-text body on 400 and 413 log refusals ([70df680](https://github.com/VarroaCI/varroa-jenkins/commit/70df6800d0090d11e9cd930b26c1d3e92519a502))
* **mite:** count loopback waits from handler entry ([6f1d670](https://github.com/VarroaCI/varroa-jenkins/commit/6f1d67056881df4e94748117d278939ede808c08))
* **mite:** derive the restart-copy ack wait from the claim budget ([20c2d17](https://github.com/VarroaCI/varroa-jenkins/commit/20c2d170351c9a04feedbd0addfdf9af7ed4cdff))
* **mite:** hold restart-copy answers until the ack arrives ([73dcfcf](https://github.com/VarroaCI/varroa-jenkins/commit/73dcfcfab73b74b592c4b8125986dbf64b5eb299))
* **mite:** publish a mesh run.state envelope before its ack ([73be627](https://github.com/VarroaCI/varroa-jenkins/commit/73be6275bf9d02457768ccd3a3da3ac269929681))
* **mite:** refuse an unresolved run.state tenant after a grace ([125dc72](https://github.com/VarroaCI/varroa-jenkins/commit/125dc7245c2acf127dddc9bdcc01855a15c5d525))
* **mite:** reload a branch job through the plugin and keep one failure per tenant ([1c38a88](https://github.com/VarroaCI/varroa-jenkins/commit/1c38a88197418e1c3f32f2d9e0ed73c4489808b7))
* **mite:** send a body with run-state 400 and 413 refusals ([6dcfc5e](https://github.com/VarroaCI/varroa-jenkins/commit/6dcfc5eb4d38bef7ce7e726ab964f383ced98d2a))
* **plugin:** drop a run-state record the mite refuses with a 4xx ([0b89712](https://github.com/VarroaCI/varroa-jenkins/commit/0b897128cb55b1fc977763fd8dc3786d2ebc4f73))

## [0.4.0](https://github.com/VarroaCI/varroa-jenkins/compare/v0.3.0...v0.4.0) (2026-10-15)


### Features

* **auth:** accept declared local accounts beside OIDC ([19c3cae](https://github.com/VarroaCI/varroa-jenkins/commit/19c3caedcbbff566852439b549b4bb082f74a147))
* cell visibility — CellPool CRD, cell usage reads, operator cell views, team cell card, pipelines folder tree ([9fcb1c3](https://github.com/VarroaCI/varroa-jenkins/commit/9fcb1c32f8a860e2f5807beab15ec4a0722b3c26))
* **cells:** approve script signatures per cell pool ([b12131c](https://github.com/VarroaCI/varroa-jenkins/commit/b12131c1dcfb26814053ab050132b0c185cd7f61))
* **cells:** configure pool-level SMTP for cells ([a2fb574](https://github.com/VarroaCI/varroa-jenkins/commit/a2fb574ca59dad4d5b0497183ce72e4531f94d74))
* **cells:** declare trusted shared libraries on a cell pool ([e168a18](https://github.com/VarroaCI/varroa-jenkins/commit/e168a18a3098a4b6f11deb407cc9e9ca76991d14))
* **cells:** issue a prefix-scoped STS artifact credential to each cell ([b591c3d](https://github.com/VarroaCI/varroa-jenkins/commit/b591c3db0a0390e185dde778ac5c40d169a79d5a))
* **cells:** lift tenant pod templates onto the tenant cloud ([8b8010e](https://github.com/VarroaCI/varroa-jenkins/commit/8b8010e86db94884627e4008b50bcd4826d22296))
* **cells:** project a GitHub App tenant credential for checks ([7eef3ec](https://github.com/VarroaCI/varroa-jenkins/commit/7eef3ecfb3615b617c206548c1fe5266303a34dc))
* **cells:** project an sshPrivateKey tenant credential ([0896ceb](https://github.com/VarroaCI/varroa-jenkins/commit/0896ceb56e071881b67e716e29a18aeae92f92ab))
* **cells:** project secretFile and certificate tenant credentials ([33fc70a](https://github.com/VarroaCI/varroa-jenkins/commit/33fc70a072ca1db0f24f776bfc896b3e9377dc62))
* **cells:** reject an inline Job DSL step in a tenant manifest ([126f241](https://github.com/VarroaCI/varroa-jenkins/commit/126f241fd6bbb4cdd8b93e678d4b5c98f7a5a3dc))
* **cells:** resolve a pool cell's artifact Secret and gate the pool fill ([9782e78](https://github.com/VarroaCI/varroa-jenkins/commit/9782e785b18762b9f34ed4a6c49adca75de18701))
* **cells:** seed a default cell template on an auto-created pool and stamp the pool label ([01723ca](https://github.com/VarroaCI/varroa-jenkins/commit/01723cacd924c7eebac12e64581271c1091f40a5))
* **cells:** share the pool confidential store and open the slot 0 agent door ([cccf290](https://github.com/VarroaCI/varroa-jenkins/commit/cccf29047a7a6f1795ac2df3dbab847a20fbf0bb))
* **cells:** show refused script signatures on the cell and the build ([0e6ff8f](https://github.com/VarroaCI/varroa-jenkins/commit/0e6ff8ffd2337e35c42ce36cbea7e4a4528d2b9c))
* **export:** replay each private commit to the public repo ([a022bd0](https://github.com/VarroaCI/varroa-jenkins/commit/a022bd05d248118ee951ef3be27fe21611bc9323))
* fleet attention signals, NATS credential rotation, MCP catalog coverage ([#9](https://github.com/VarroaCI/varroa-jenkins/issues/9)) ([558b5c0](https://github.com/VarroaCI/varroa-jenkins/commit/558b5c093bb4821839f85864130ac8f1bd62120f))
* **forge:** support Gitea and Forgejo sources and the gitea webhook ([db5277c](https://github.com/VarroaCI/varroa-jenkins/commit/db5277c12efa07a0154c6842eb8a384b7d4206cd))
* **frontend:** add a force release action to the fleet locks page ([b3b1d8d](https://github.com/VarroaCI/varroa-jenkins/commit/b3b1d8d702596ec16c4ad80b61ac44d8739db24d))
* **frontend:** add build page orientation ([1d8ffd0](https://github.com/VarroaCI/varroa-jenkins/commit/1d8ffd04128b2d7b79c4c8a0122e2d0f8f297f3c))
* **frontend:** create and show OCI catalog sources in the dashboard ([6051800](https://github.com/VarroaCI/varroa-jenkins/commit/6051800e623dad2a1ae276581657ec6ce4772f93))
* host a pipelines tenant on a member cluster ([29ced4b](https://github.com/VarroaCI/varroa-jenkins/commit/29ced4b551d79bc510423861c349bb1ae5079ecf))
* **items:** render the githubSkipNotifications trait on GitHub sources ([38d6c3f](https://github.com/VarroaCI/varroa-jenkins/commit/38d6c3fff4f77cec3cf3884324bdd25b1a03a858))
* **items:** support private and non-GitHub folder libraries ([4d05d93](https://github.com/VarroaCI/varroa-jenkins/commit/4d05d931dd4ef2134b422d16ac5a277fa8cc2e75))
* **locks:** add an operator force release for a releasing fleet lock ([abd9663](https://github.com/VarroaCI/varroa-jenkins/commit/abd966333f2b89708590f5a51cb38e943e5a37bb))
* **mcp:** report the fan-out status in list_controllers ([83222f9](https://github.com/VarroaCI/varroa-jenkins/commit/83222f94831052521fd3bc679bf108e8bf5668d4))
* **mesh:** forward a tenant's triggers to each job's home slot ([4a66dd7](https://github.com/VarroaCI/varroa-jenkins/commit/4a66dd7f87786c1986f84db06d9e62f789fe3e49))
* **mesh:** key a run-state object by the process that pushed it ([a252e95](https://github.com/VarroaCI/varroa-jenkins/commit/a252e951d4a94bb8ae94c0489115b123af9353bd))
* **mesh:** re-dispatch a resume whose command failed on its target ([1d94d14](https://github.com/VarroaCI/varroa-jenkins/commit/1d94d14d4b3dbbbb97ead3818a845d71f8b21433))
* **mesh:** resume a lost pipeline build on its new cell ([9dbc947](https://github.com/VarroaCI/varroa-jenkins/commit/9dbc947e32cafcd2c012462dec0a73edd3f9f11c))
* **nats:** encrypt the JetStream store at rest ([a9c306b](https://github.com/VarroaCI/varroa-jenkins/commit/a9c306bb900aa5b7865390a3a4f40c3c8ccc56c0))
* **operator:** grant the BFF a read on each composed bundle's content ConfigMap ([4148aa0](https://github.com/VarroaCI/varroa-jenkins/commit/4148aa0c1013acc869b9cf24bf9b0b14675c2427))
* **pipelines:** add per-tenant and per-job record-store retention ([7f19a30](https://github.com/VarroaCI/varroa-jenkins/commit/7f19a30abf716f31dfc154160044caa7406c04a7))
* **pipelines:** add the run failure digest and the varroa.summary step ([e836ba0](https://github.com/VarroaCI/varroa-jenkins/commit/e836ba07d4d267ae2720d6936133fdb9189b661e))
* **pipelines:** answer a paused input step from the dashboard ([12254f5](https://github.com/VarroaCI/varroa-jenkins/commit/12254f5a0b1890c2531a4cc33b78c755c2678814))
* **pipelines:** cap concurrent builds per tenant and per cell ([606ba28](https://github.com/VarroaCI/varroa-jenkins/commit/606ba28f58bcdf6f87a4655208f42c69cf22a9be))
* **pipelines:** capture how each run was defined ([8012d95](https://github.com/VarroaCI/varroa-jenkins/commit/8012d95ebd4d2e6807ead4e552fbdd7139740921))
* **pipelines:** link a run's shared library to its repository ([6c6d13f](https://github.com/VarroaCI/varroa-jenkins/commit/6c6d13f9426ca518a67501ecafe4c9cc9949cc50))
* **pipelines:** link build metadata and show run origin ([e334440](https://github.com/VarroaCI/varroa-jenkins/commit/e334440b7419edac2dcd97b47839d0ce1d949cae))
* **pipelines:** link job card repositories on Gitea hosts ([41848d0](https://github.com/VarroaCI/varroa-jenkins/commit/41848d0db80c46593095a6b5d74804127a311fe6))
* **pipelines:** link runs to the runs that started them ([563e0cb](https://github.com/VarroaCI/varroa-jenkins/commit/563e0cb0d9a829d856ed520aaac3969f8c424348))
* **pipelines:** list the branches and change requests of a multibranch job ([789c2a1](https://github.com/VarroaCI/varroa-jenkins/commit/789c2a1f9d889c2590e7fab8a5633094209f09e1))
* **pipelines:** mark a declared job pending until its cell holds it ([e5f2272](https://github.com/VarroaCI/varroa-jenkins/commit/e5f2272167a66f386026c07f92daed6a5e3925f8))
* **pipelines:** name the bundle input that declared each job ([6be7a0b](https://github.com/VarroaCI/varroa-jenkins/commit/6be7a0b5a45f9a07a046f574034df9e5b7e6473b))
* **pipelines:** name the forge of a run revision ([aff0101](https://github.com/VarroaCI/varroa-jenkins/commit/aff0101bde52903a4fe683e2ef9f34ec6b8d99cc))
* **pipelines:** open the cell card from a pill and show queue wait ([aee17dd](https://github.com/VarroaCI/varroa-jenkins/commit/aee17dd2830182906064222adcd4cf39ff27f6cb))
* **pipelines:** record per-suite test counts for a run ([06e861d](https://github.com/VarroaCI/varroa-jenkins/commit/06e861d90c56d58d8526fe5dc025d270fe1cb7a5))
* **pipelines:** replay a completed build with an edited script ([4332f69](https://github.com/VarroaCI/varroa-jenkins/commit/4332f69464a01e950af3765e8e5dd40dbc3ecb30))
* **pipelines:** restart a declarative run from a stage ([3a4b24f](https://github.com/VarroaCI/varroa-jenkins/commit/3a4b24fe2bbdf1c0d8c96cc22127356f0bff0c8a))
* **pipelines:** scan a multibranch job ([9c8b754](https://github.com/VarroaCI/varroa-jenkins/commit/9c8b754fe1b2599a5e956b76f6153a7633bc3ac3))
* **pipelines:** serve fleet lock reads, a lock notification, and the lock views ([6b9d8c6](https://github.com/VarroaCI/varroa-jenkins/commit/6b9d8c6a843143d6594c9067f780c23a877044fa))
* **pipelines:** show the change request of a run ([3c38eff](https://github.com/VarroaCI/varroa-jenkins/commit/3c38effb8b528c0d254281518e658c95187870ed))
* **pipelines:** stream live run notifications to the pipelines pages ([d5abedb](https://github.com/VarroaCI/varroa-jenkins/commit/d5abedbb2e0435b920fce7974d1f36f1ce2abfa3))
* **plugin:** emit run.source a third time at the first agent allocation ([60d7e2d](https://github.com/VarroaCI/varroa-jenkins/commit/60d7e2ded88381d13734598207b569be9d1cdfd4))
* **plugin:** end a lost freestyle build at Jenkins boot ([9e86cb4](https://github.com/VarroaCI/varroa-jenkins/commit/9e86cb43fafdcee878aab5df06643098324b2ad2))
* **pluginlock:** pin pipeline-githubnotify-step for plain-job commit status ([3c5f996](https://github.com/VarroaCI/varroa-jenkins/commit/3c5f99697207ed1f21137afec522f33e658cccec))
* **recorder:** confirm a cell loss from two signals and re-fire lost intents ([ebbf24e](https://github.com/VarroaCI/varroa-jenkins/commit/ebbf24ede24c26e104acdaf2a99db51925a09c04))
* **recorder:** record an accepted build trigger and mark a queued run its cell lost ([5842ca4](https://github.com/VarroaCI/varroa-jenkins/commit/5842ca4ca426a731fbf83e1e7d6a75203dfe66c1))
* **recordstore:** add the tenant completion webhook ([8e1e9ea](https://github.com/VarroaCI/varroa-jenkins/commit/8e1e9eaab0244b68101a378127837a95b93b48ff))
* **recordstore:** read a tenant's live job inventory ([b7abf09](https://github.com/VarroaCI/varroa-jenkins/commit/b7abf09fb6942430dd4c58a31e2caef41fad1274))
* **restart:** keep a restart copy of a finalized cell run ([86cf7ed](https://github.com/VarroaCI/varroa-jenkins/commit/86cf7ed62e16d353260958b46cf3070c9663c8c9))
* **restart:** restore a restart copy when the cell lost the source build ([c867507](https://github.com/VarroaCI/varroa-jenkins/commit/c867507525f7ef7eb6c8e31ce36264aec38126f1))
* **search:** report the read status of each cluster ([bd968f0](https://github.com/VarroaCI/varroa-jenkins/commit/bd968f0eb99bd1cf25ad005c4a088d1e3f001dbe))
* shared Jenkins cells, external build state, pipelines, cross-tenant calls, and external runners ([7876c9c](https://github.com/VarroaCI/varroa-jenkins/commit/7876c9c5c37822fa58c43ce5cc3f2ca7797667bd))
* show the reason a build trigger failed on the run page ([8d066db](https://github.com/VarroaCI/varroa-jenkins/commit/8d066db76a0bf3094ee0b8ffa5d91dc8e5b1121a))
* **spec-editor:** show ingress and mite sidecar in the curated form ([b564410](https://github.com/VarroaCI/varroa-jenkins/commit/b5644108b09f31a6f23ffbb7b79f54e76db91ca3))
* store test cases and serve a test case history (add-run-test-cases) ([eb76cb3](https://github.com/VarroaCI/varroa-jenkins/commit/eb76cb361f42c56b4461a8074eae26cce9303a96))
* **tenant:** place a pipelines tenant across active slots and standbys ([74e7bfe](https://github.com/VarroaCI/varroa-jenkins/commit/74e7bfec6fcd7e0e42153208a3a15525bbb11b8a))
* **update-center:** prefetch servable declared plugin gaps ([b9f81d8](https://github.com/VarroaCI/varroa-jenkins/commit/b9f81d80c2041062bbeac342ac22c9f43d965884))
* **updatecenter:** report latest servability against the requesting core ([e2f9e0a](https://github.com/VarroaCI/varroa-jenkins/commit/e2f9e0a14acabb081471a598bc70e3c3e54fcde3))
* **updatecenter:** resolve a latest pin against the requesting core version ([71a053c](https://github.com/VarroaCI/varroa-jenkins/commit/71a053c41fc4eb58fb22d6ca1b6574d84b84a846))
* **varroactl:** add locks list ([f4ba89e](https://github.com/VarroaCI/varroa-jenkins/commit/f4ba89e8d7c1e1157f48e66c98493af838268c25))
* **webhooks:** fence the tenant door by forge and team token ([bcfa6f1](https://github.com/VarroaCI/varroa-jenkins/commit/bcfa6f11567ee01c42f5c6b1739f4705e3bddc50))


### Bug Fixes

* **activity:** drop non-activity live frames and map auth icons ([4e3786e](https://github.com/VarroaCI/varroa-jenkins/commit/4e3786ec1dd4461adc9c158a57589bf084c68460))
* **api:** read config with each cluster's operator namespace ([bf85163](https://github.com/VarroaCI/varroa-jenkins/commit/bf85163b8357e80c93d3bff0f6fd71d351b55863))
* **api:** read the retained build record through the range reader ([0e41a6d](https://github.com/VarroaCI/varroa-jenkins/commit/0e41a6da4051741580d05317751ece5175922395))
* **api:** redact credential values in the controller diff read ([04ae0a6](https://github.com/VarroaCI/varroa-jenkins/commit/04ae0a6c51e526058dca515116b052960215bfba))
* **api:** require profile and agentNamespace in the TeamPipelines contract ([75c527c](https://github.com/VarroaCI/varroa-jenkins/commit/75c527cd10197bba81d6d2e3a1ebcc808fd92a84))
* **api:** scope catalog and bundle reads to readable namespaces ([f998575](https://github.com/VarroaCI/varroa-jenkins/commit/f9985755ce374f54f024fd6eeac71547a1311bd5))
* **api:** show a generic trigger on the job Triggers card ([f379432](https://github.com/VarroaCI/varroa-jenkins/commit/f379432c03413267e8835bad9795a11658569043))
* **auth:** read the configured OIDC group and user claims ([82ae1ad](https://github.com/VarroaCI/varroa-jenkins/commit/82ae1ad1265a64734152f8c94177eb0bd3f8c9b5))
* **bff:** check the configured group claims before the zero-groups warning ([7687d3b](https://github.com/VarroaCI/varroa-jenkins/commit/7687d3b7bff8412e4897ebdda2f0a918d0b8fabe))
* **bff:** end brood controller reads when the request ends ([3a934dc](https://github.com/VarroaCI/varroa-jenkins/commit/3a934dcb0759979020215ad1334c55021feeb063))
* **bff:** fill a live log tail gap from the blob store ([92b2710](https://github.com/VarroaCI/varroa-jenkins/commit/92b271052a1cd6980f149e8e06e06fe0023ac7f3))
* **bff:** list a placed Team's jobs from its own tenant bundle ([ff2d1b4](https://github.com/VarroaCI/varroa-jenkins/commit/ff2d1b472676414edd936ba7c7e16527ad5cb564))
* **bff:** list, read, and scale cell pools on every cluster of the brood ([39ad66a](https://github.com/VarroaCI/varroa-jenkins/commit/39ad66ae07474c75d2e23ed853da2432f29a0cbd))
* **bff:** page the member controllers list from the operator cache and follow the request context ([5aa30b7](https://github.com/VarroaCI/varroa-jenkins/commit/5aa30b7ab63da8b001aa2214c3360c3894046f19))
* **bff:** page the remote controllers list and show a member that did not answer ([756c6b5](https://github.com/VarroaCI/varroa-jenkins/commit/756c6b5e96ccc279608a4a9bd4ac987c73f2fe38))
* **bff:** pass the interrupted reason of a run through to the build page ([38ef361](https://github.com/VarroaCI/varroa-jenkins/commit/38ef361847fdc3d0bc6d8bf737e70463a5148cd6))
* **bff:** pass the request context to the controller endpoint read and read the defaults once per list ([21cd8d3](https://github.com/VarroaCI/varroa-jenkins/commit/21cd8d37918e8780e9c758b69c1dcbab1b805c2a))
* **bff:** pass the request context to user, group, team, runner and controller reads ([a6e7dc7](https://github.com/VarroaCI/varroa-jenkins/commit/a6e7dc76a7485c4f235b83205f1d38329a292dea))
* **bff:** read the cause description the gateway writes as a string ([52f7ae2](https://github.com/VarroaCI/varroa-jenkins/commit/52f7ae2348f8cd5d11e1f0edc409995c17a00c7c))
* **bff:** recover the live log tail subscription after a denial ([d848948](https://github.com/VarroaCI/varroa-jenkins/commit/d84894841d5331ff705be9bfd7076874e6ef18fe))
* **bff:** refuse writes to the reserved update-center catalog source ([a1920ee](https://github.com/VarroaCI/varroa-jenkins/commit/a1920ee486c494fbb960111f271012e1890b3837))
* **bff:** send config, brood-op and brood-schedule reads with the request context ([af64f9e](https://github.com/VarroaCI/varroa-jenkins/commit/af64f9e453f51df9438d50cf064b61653ed95558))
* **bff:** show a member cluster's own controller endpoint ([944564a](https://github.com/VarroaCI/varroa-jenkins/commit/944564adad03bee60fb69a54233b514fb91d3a67))
* **bundle:** name the size and the limit when composed content is too large ([77d6710](https://github.com/VarroaCI/varroa-jenkins/commit/77d67109af808410810b091f9503aa717661f099))
* **bundle:** report top-level item keys the composer drops ([e07b8b9](https://github.com/VarroaCI/varroa-jenkins/commit/e07b8b97fe7ca23615d412718d0e80ab07bafacf))
* **bundle:** stop git fetches at the compose and catalog deadlines ([0bac28c](https://github.com/VarroaCI/varroa-jenkins/commit/0bac28c7d1d9b48db86bdc97fbc34b6aae458de7))
* **bus:** cancel the NATS request when the context ends ([d2bfea8](https://github.com/VarroaCI/varroa-jenkins/commit/d2bfea8dfa835e6cb1795e706cfdbcee9a5b863d))
* **bus:** recover the operator and BFF subscriptions after a denial ([60c8334](https://github.com/VarroaCI/varroa-jenkins/commit/60c833433a76a0675194b7c43f45117aa2599a30))
* **bus:** send the desired state as a request that NATS does not store ([5c44c55](https://github.com/VarroaCI/varroa-jenkins/commit/5c44c5569f60b2a6041df74d6b046cbfed0af8a2))
* **catalog:** show the OCI source of a catalog source ([3892cc3](https://github.com/VarroaCI/varroa-jenkins/commit/3892cc325e13c167c6474274a73f25b8793b99ca))
* **cells:** confirm the old pod of a left cell so its fleet lock releases ([eb73ede](https://github.com/VarroaCI/varroa-jenkins/commit/eb73ede0193cb709b19b511cdec1543681ee7a7b))
* **cells:** declare pool Gitea servers on every cell ([1bfff19](https://github.com/VarroaCI/varroa-jenkins/commit/1bfff19b39747705dd337d510a43d6307301d930))
* **cells:** delete a stale Pending agent pod fast and keep an adoptable one ([143116a](https://github.com/VarroaCI/varroa-jenkins/commit/143116ad2428a40a646cfc3380e24da7ed99f3a4))
* **cells:** derive a dead instance in the recorder when a member instance changes ([a1e4987](https://github.com/VarroaCI/varroa-jenkins/commit/a1e4987ea4e3dd6f3968d39b0bdfa3e9f0f0b49d))
* **cells:** exclude job-dsl from the cell plugin set ([9943ad9](https://github.com/VarroaCI/varroa-jenkins/commit/9943ad9431ab03e91f5fe986b74ed175205a1f4e))
* **cells:** hold one tenant folder when its bundle content fails a check ([9320973](https://github.com/VarroaCI/varroa-jenkins/commit/9320973ff53131ab8ea500f6ab7c0a837adf8e47))
* **cells:** hold the unready roll while plugins-init runs and bound the delete of an init-stage pod ([cfc971d](https://github.com/VarroaCI/varroa-jenkins/commit/cfc971dbe184a14b520ceba5ece0781be6dbcf2a))
* **cells:** reap tenant agent pods that no live cell refreshes ([60ada53](https://github.com/VarroaCI/varroa-jenkins/commit/60ada53a6308a7bd5d278d933f21e7f9ab5a49e9))
* **cells:** refuse unknown keys in tenant item manifests ([763240a](https://github.com/VarroaCI/varroa-jenkins/commit/763240a020fcc7ba4d4e0b98b67ba6dffa8b6189))
* **cells:** render a cell egress rule for each pool library remote ([e718e9d](https://github.com/VarroaCI/varroa-jenkins/commit/e718e9d5b30c80c3f22cd07b7b681334f01f5a7c))
* **cells:** report a replaced cell instance on every placement announce ([8986e4a](https://github.com/VarroaCI/varroa-jenkins/commit/8986e4a692c302470430eb7eb01b7a62161ea072))
* **cells:** send a tenant credential value to Jenkins as written ([c2ab7ab](https://github.com/VarroaCI/varroa-jenkins/commit/c2ab7abfd71ea6abd70f390c804338a3f8524e57))
* **chart:** give the NATS pods the same pull Secrets as every other pod ([92055ff](https://github.com/VarroaCI/varroa-jenkins/commit/92055ffd50828993de3da39e58c6a7e0ea69b1a1))
* **chart:** let controller pods reach the BFF for local issuer keys ([c11451e](https://github.com/VarroaCI/varroa-jenkins/commit/c11451ee72ad21ce19b65e5482015e6a97801772))
* **chart:** refuse a cells.maxTenantsPerCell value below one at render time ([f4a60b8](https://github.com/VarroaCI/varroa-jenkins/commit/f4a60b888412ee7c7c7a132258ec548eef3bd3d5))
* **chart:** start the JetStream key with a letter ([e7e3903](https://github.com/VarroaCI/varroa-jenkins/commit/e7e39031245a8d5e756fa7ad6853d29c9074a39d))
* **claims:** cancel a replay or restart on NOT_HOME, never forward it ([52053d1](https://github.com/VarroaCI/varroa-jenkins/commit/52053d181c32a8cf6aef075f3eb7d8f365d6bf86))
* **claims:** retry a claim reply with no decision and expire an unclaimed queued run ([4955e55](https://github.com/VarroaCI/varroa-jenkins/commit/4955e5547186bab2b23ea58df6585064b753067d))
* close the live-smoke follow-ups from the jenkins-cells epic ([7336f70](https://github.com/VarroaCI/varroa-jenkins/commit/7336f70f203f2e6f93f02641b9b69b6fcb78ce79))
* **controller:** call ucmeta.IsLatest in the candidate servability check ([0174445](https://github.com/VarroaCI/varroa-jenkins/commit/01744452974fa099171f675c77bc41da83ba52ed))
* **controller:** check candidate servability through the update center ([0c697c1](https://github.com/VarroaCI/varroa-jenkins/commit/0c697c10e6efc8d23fb2fed093763c81ccf8c4bb))
* **controller:** report a refused items bundle as ItemsRefused ([2ed66b3](https://github.com/VarroaCI/varroa-jenkins/commit/2ed66b3c2389479e2ab65992b19bb183e231cbf6))
* **controller:** run the cell lint over tenant pipeline-template bodies ([91b9cc2](https://github.com/VarroaCI/varroa-jenkins/commit/91b9cc2344d1e26169561842b7dd10993d937c73))
* **crd:** make the controller cell role immutable at the spec level ([a776695](https://github.com/VarroaCI/varroa-jenkins/commit/a7766956e878fe95b7e1d832e91d8cff9c5faa75))
* **crd:** make the controller ingress mode immutable at the spec level ([ad09603](https://github.com/VarroaCI/varroa-jenkins/commit/ad096035248a4a9c51c22b5c35d646a23ac06673))
* **export:** clear the public export gate hits on main ([0bfe08d](https://github.com/VarroaCI/varroa-jenkins/commit/0bfe08dc2df0894469c371baeaa5712105663a67))
* **export:** key gate allowlists by bare path and run the gate in CI ([4602461](https://github.com/VarroaCI/varroa-jenkins/commit/46024612ef214ca27e241fe174d5f562b95afb12))
* **frontend:** give the tab row tablist and tab roles ([7edb4ee](https://github.com/VarroaCI/varroa-jenkins/commit/7edb4eef35401c0418aa1c1dca9ab72dc923ac53))
* **frontend:** keep Admin & access lists inside their card ([a328af8](https://github.com/VarroaCI/varroa-jenkins/commit/a328af8d46d84419863982f02cbab53f92a3b8d0))
* **frontend:** keep the local tree out of the image build context ([8e221e3](https://github.com/VarroaCI/varroa-jenkins/commit/8e221e35a6de8ee54e1d4b2a3dd50e23f8d9cfc8))
* **frontend:** keep the log resume position after a gap frame ([98b7362](https://github.com/VarroaCI/varroa-jenkins/commit/98b73623b912c44d0fec44c03cd6b2f1a129d6a2))
* **frontend:** render cell.enabled read-only in the spec editor ([ef8c70e](https://github.com/VarroaCI/varroa-jenkins/commit/ef8c70e8a9e1a4d9a081c0036ad13176547973d2))
* **frontend:** scroll the page to a log permalink and hide the writing notice for a finalized log ([7485ecd](https://github.com/VarroaCI/varroa-jenkins/commit/7485ecd41a32d7bfa9c31ad96b7a507e96ec6c7d))
* **frontend:** show a loading state on a gated route until permissions load ([10e1b05](https://github.com/VarroaCI/varroa-jenkins/commit/10e1b058e20a94cbf8295d07a2cc53bbad868069))
* **frontend:** show the server's message when a cell pool scale write is refused ([b9e5ac3](https://github.com/VarroaCI/varroa-jenkins/commit/b9e5ac3d70158af3b4e711bde4b575dd5348a628))
* **frontend:** turn follow off when a log permalink resolves ([da48918](https://github.com/VarroaCI/varroa-jenkins/commit/da489184d1096c3d96f83854fa9593eca8450f26))
* **gateway:** fence a lost cell's late log part write in the store ([a58e59d](https://github.com/VarroaCI/varroa-jenkins/commit/a58e59d6c93cf0e145b62a05663cdd5c9b8f2cb9))
* **gateway:** fence an in-flight log Overwrite and index append ([456b552](https://github.com/VarroaCI/varroa-jenkins/commit/456b552a62c00de0be2cdf6ae25ee55614b9b4cd))
* **gateway:** give the imperative consumer an explicit ack wait ([0477c46](https://github.com/VarroaCI/varroa-jenkins/commit/0477c46f4428d708e0788e4885b128942de4acb4))
* **gateway:** mark a moved writer's chunk on an old part as a gateway gap ([e8be336](https://github.com/VarroaCI/varroa-jenkins/commit/e8be336ef575db7f219d09c371641258eaad2782))
* **gateway:** pace the log budget pause by the drain rate ([f645be7](https://github.com/VarroaCI/varroa-jenkins/commit/f645be752dfc52462493bc7496a311ecdba8da03))
* **gateway:** redeliver an in-flight imperative after the mite disconnects ([f6b3423](https://github.com/VarroaCI/varroa-jenkins/commit/f6b34234c8107e499f18f5fd01fd10b88c8a86e7))
* **gateway:** replace the log parts a lost writer left above a hole ([b447c56](https://github.com/VarroaCI/varroa-jenkins/commit/b447c569103ce5d79cddb14b1f5da2be46cd2109))
* **gateway:** subscribe again when NATS rejects a mite subscription ([8a7851e](https://github.com/VarroaCI/varroa-jenkins/commit/8a7851e750cb9477dfe12eb8e4fc161b10285f3c))
* **gateway:** trace every run command outcome with its actor ([7cded1e](https://github.com/VarroaCI/varroa-jenkins/commit/7cded1e5ff77782d6c7c0dbc62b6f34273695152))
* **gateway:** write one recovered index entry for each log part ([cee974d](https://github.com/VarroaCI/varroa-jenkins/commit/cee974d19ffe743c2d46dc7ab80cde69b5076805))
* **headless:** point the headless location URL at the dashboard door ([73a7a0d](https://github.com/VarroaCI/varroa-jenkins/commit/73a7a0d7f4990cecc2870eb94eb07ccf558c2933))
* **items:** check folder library credentials on a non-cell controller ([8a3e796](https://github.com/VarroaCI/varroa-jenkins/commit/8a3e796362234134c4454d4e5d821eee3003c6f1))
* **items:** omit the genericTrigger token from the tenant refusal ([ca62650](https://github.com/VarroaCI/varroa-jenkins/commit/ca62650fcae3fd4c0f39e80d1fc14c750b72d07f))
* **items:** render item-level parameters and validate durabilityHint ([446db71](https://github.com/VarroaCI/varroa-jenkins/commit/446db71dda8bfc57eff7cd35f1f8ca39974ccb43))
* **items:** render orphanedItemStrategy on a multibranch item ([b8b60f4](https://github.com/VarroaCI/varroa-jenkins/commit/b8b60f4a9d13bf4a81c968ef7171f4342bcbf885))
* **items:** render pipeline item triggers and reject them on kinds that do not build ([815b7d4](https://github.com/VarroaCI/varroa-jenkins/commit/815b7d47669931c18feb6098e0ee36a1d5f7002f))
* **items:** render the declared GitHub branch source traits ([eca0f49](https://github.com/VarroaCI/varroa-jenkins/commit/eca0f49e9a47eff761094d4190d0bba2aab9ba9e))
* **items:** write the spec element on a varroaUpstream trigger ([db160b3](https://github.com/VarroaCI/varroa-jenkins/commit/db160b3ee02030c5c63875ffcb8e214d09343e7a))
* **ledger:** keep build numbers consistent through a claim burst into a newly placed tenant ([4e4c5b0](https://github.com/VarroaCI/varroa-jenkins/commit/4e4c5b0f29c14c93d2a47b2312f53dbf8a7ed456))
* **ledger:** mark the start of a build before it leaves the queue ([76f4ee8](https://github.com/VarroaCI/varroa-jenkins/commit/76f4ee8b140144f0204c7fac3aa39daa9bf6824b))
* **login:** mount a new submit button when the local form opens ([e2e2759](https://github.com/VarroaCI/varroa-jenkins/commit/e2e27590caa5c59aa1160dc0699ced590daa40dd))
* make three tests deterministic, retire dead NATS values, run chart tests in CI ([fa79a66](https://github.com/VarroaCI/varroa-jenkins/commit/fa79a663992f9e5428f23d9b03a01dbb7280f69c))
* **mesh:** continue the log sequence of a build that moves to another cell ([533f8ec](https://github.com/VarroaCI/varroa-jenkins/commit/533f8ecb66a096600e8b8fe45aaa5c38b6faa478))
* **mesh:** key a run-state object by the push that wrote it ([69e2d99](https://github.com/VarroaCI/varroa-jenkins/commit/69e2d99e7bef0ecc1c838b5bb9c6d706d1adec1f))
* **mesh:** push run state from the process that resumes a build ([eec6106](https://github.com/VarroaCI/varroa-jenkins/commit/eec61062b0092e9452564f3771f7758388151b9f))
* **mite:** close only the connection of the session that failed a send ([0a6614b](https://github.com/VarroaCI/varroa-jenkins/commit/0a6614b662155a224481e8111e23d83f43ce1967))
* **mite:** drop rehydration records of jobs the items apply did not create ([80dd265](https://github.com/VarroaCI/varroa-jenkins/commit/80dd265c5407651a79fa69a5f8b682481a775699))
* **mite:** list the plugin activity types in the proto comment ([a7de032](https://github.com/VarroaCI/varroa-jenkins/commit/a7de03205cb76206ba581c0da77f2114c76e2d31))
* **mite:** name the time left in a busy pause on a loopback 503 ([7c9663e](https://github.com/VarroaCI/varroa-jenkins/commit/7c9663e68847be48849d3202f3a6aa55c8424926))
* **mite:** omit the Bearer on generic trigger replays ([58508d7](https://github.com/VarroaCI/varroa-jenkins/commit/58508d71b750ab15d5d2b96b342656dd0c35af72))
* **mite:** re-send a log chunk after a BUSY pause on a live stream ([1a6641a](https://github.com/VarroaCI/varroa-jenkins/commit/1a6641a9f5674061a44b0379156b9ca01c1c9254))
* **mite:** retry a trigger until the cell holds the job ([bb1753f](https://github.com/VarroaCI/varroa-jenkins/commit/bb1753f58d65063acd981bf7714c336406194259))
* **mite:** seed jobs the items apply creates and never mark a deferred bundle converged ([4b3a0b4](https://github.com/VarroaCI/varroa-jenkins/commit/4b3a0b449c83ca87261a906599bff14951150da0))
* **mite:** stamp the build version into the mite binary ([a48e602](https://github.com/VarroaCI/varroa-jenkins/commit/a48e6029efc3465d288d21e46cd9bc4db1cf448d))
* **mite:** wait for a newly placed tenant's items before a trigger on a running cell ([c56de95](https://github.com/VarroaCI/varroa-jenkins/commit/c56de956da4b31d212ad68099a2cd7031f72fe68))
* **mite:** wait for the items apply before a trigger after a reconnect ([ba0788f](https://github.com/VarroaCI/varroa-jenkins/commit/ba0788f9c47bc5e98db49e05d605cb6043f2e4f4))
* **operator:** delete the agent ServiceAccount, Role, and RoleBinding when a Controller is deleted ([0c58d2a](https://github.com/VarroaCI/varroa-jenkins/commit/0c58d2aa1059b715de11b8ef808b4df602baace2))
* **operator:** drop the mite subscriptions of a released shard ([7f50906](https://github.com/VarroaCI/varroa-jenkins/commit/7f5090662c86630ceebf42c01ecf8cd8f233d40f))
* **operator:** drop the shards whose leases a replica cannot confirm ([52a267c](https://github.com/VarroaCI/varroa-jenkins/commit/52a267cc45c47879d4c8668fb0f3be50c1f6ba89))
* **operator:** fence the StatefulSet scale-down of a stopped or hibernated tick ([f949c63](https://github.com/VarroaCI/varroa-jenkins/commit/f949c63301a4dede1174ca914a3a08e5957ac89e))
* **operator:** fence the StatefulSet scale-up of a provisioning tick ([a94023d](https://github.com/VarroaCI/varroa-jenkins/commit/a94023d27b33dc6565acd978e789fe47e1fada52))
* **operator:** frame artifact Secret values in the secret hash ([6feece7](https://github.com/VarroaCI/varroa-jenkins/commit/6feece780cf0046ef6488d4e0388653ba30e2fad))
* **operator:** frame each part of the composite desired-state hash ([3928620](https://github.com/VarroaCI/varroa-jenkins/commit/392862070ef32ae51680d019df16bb45a67fd46b))
* **operator:** frame tenant homes and update-center digest inputs with JSON ([8f058e2](https://github.com/VarroaCI/varroa-jenkins/commit/8f058e252271aa9f7af75cb2bbb7dc75671ddc01))
* **operator:** hold a live controller in Running when its mite stream is lost ([7bb94f2](https://github.com/VarroaCI/varroa-jenkins/commit/7bb94f2c132947eb922d443d83ef99527938a1f8))
* **operator:** hold an in-flight rehydration push and push only a bundle ([3d2bcee](https://github.com/VarroaCI/varroa-jenkins/commit/3d2bcee963f1159cb7e1b4a90ef760b5f9e8a46a))
* **operator:** hold the uncovered jobs of a number-only round ([7fcd7a7](https://github.com/VarroaCI/varroa-jenkins/commit/7fcd7a7d4f30c3f6e91e1e7e541b23153cd91a56))
* **operator:** make the tick writes conditional on the tick's read ([22de984](https://github.com/VarroaCI/varroa-jenkins/commit/22de9842367925f4018b8318c835d291d81153f7))
* **operator:** make the tick's hibernation writes conditional ([3b75173](https://github.com/VarroaCI/varroa-jenkins/commit/3b751738fb5599d661098c2969ffa869dd3c3e02))
* **operator:** page the brood-schedule list reply and name a member whose list did not arrive ([23d92d4](https://github.com/VarroaCI/varroa-jenkins/commit/23d92d4b435357238dec9dc8b7b2e282d42e7e94))
* **operator:** page the config and brood-operation list replies ([d8f93bb](https://github.com/VarroaCI/varroa-jenkins/commit/d8f93bbea1c230bb5ea9baee9783a2a28baecbc0))
* **operator:** point controllers at the BFF issuer in ldap mode ([ef7bf01](https://github.com/VarroaCI/varroa-jenkins/commit/ef7bf016d403f5f07ebffc158893adc1d5b2b0e3))
* **operator:** push a pending rehydration bundle on a manual-mode cell ([9bc451a](https://github.com/VarroaCI/varroa-jenkins/commit/9bc451abf01a23efe61a210d94ef7e575167f7b9))
* **operator:** push a placement-only change on a manual-mode cell ([88df4a2](https://github.com/VarroaCI/varroa-jenkins/commit/88df4a228757751b4dc9a56c725564c8820caeb3))
* **operator:** refuse a plugin-roll approval that has no pending roll ([d4bd36b](https://github.com/VarroaCI/varroa-jenkins/commit/d4bd36b0f2fda7efe68d1397187b3b1bdb581a2e))
* **operator:** refuse an approval when the pending content changed ([4c212f2](https://github.com/VarroaCI/varroa-jenkins/commit/4c212f28a24b2b04373420e9c489bce20b7055c0))
* **operator:** release one deferred rehydration job, not the tenant ([dc34325](https://github.com/VarroaCI/varroa-jenkins/commit/dc34325b958c66592e66d2bec30c6141b90369a6))
* **operator:** renew due shards between the claims of a pass ([3228834](https://github.com/VarroaCI/varroa-jenkins/commit/32288342c224b4145312005ce7e1612da2700afe))
* **operator:** report a plugins-init archive fallback and a 404 download URL on the Controller ([7ba0111](https://github.com/VarroaCI/varroa-jenkins/commit/7ba0111eacfa77c2758a74bf1815edfa45b7f503))
* **operator:** roll boot-failed pods and delete the casc ConfigMap on finalize ([c92e16f](https://github.com/VarroaCI/varroa-jenkins/commit/c92e16f452e65b2ac5d325424c5045efc2cb868e))
* **operator:** route approvedeletion to the shard owner ([3b58703](https://github.com/VarroaCI/varroa-jenkins/commit/3b58703f6d1f8e39f8413645b3bc7a51957f61f9))
* **operator:** run approves for different controllers at the same time ([d5ae980](https://github.com/VarroaCI/varroa-jenkins/commit/d5ae98012297d4a37367f97cc563430aa3fa30c0))
* **operator:** run bundle compose requests at the same time ([356174b](https://github.com/VarroaCI/varroa-jenkins/commit/356174b04e7bd79085b234bbc43187c45f518f6c))
* **operator:** serialize the reconcile tick with the approve actions ([d9b1602](https://github.com/VarroaCI/varroa-jenkins/commit/d9b16023e65dde250ae4caa7648cf02157ee0a4f))
* **operator:** serve approve restart on the shard owner only ([c3a451d](https://github.com/VarroaCI/varroa-jenkins/commit/c3a451d5cad74c948d58bec18f32dfca0f35bd9d))
* **operator:** subscribe ledger actions on the operator's own cluster subject ([26ff9e3](https://github.com/VarroaCI/varroa-jenkins/commit/26ff9e3fa580e2bd2a9b0aa322f290017c05b4fd))
* **operator:** time out each shard lease API call ([d186a05](https://github.com/VarroaCI/varroa-jenkins/commit/d186a0532c82e948a1e1f61bcb67b9d557b73e65))
* **operator:** treat an existing tenant agent ServiceAccount as a quiet get-or-create hit ([927b94c](https://github.com/VarroaCI/varroa-jenkins/commit/927b94c0f5f89b0b48b595a74e25ef0f013359ec))
* **operator:** wait for the work on a shard before the lease is released ([c1ba204](https://github.com/VarroaCI/varroa-jenkins/commit/c1ba20454e5eeb27694857325f665b799a290771))
* **operator:** wake reconciles when a rehydration seed applies ([057ca7d](https://github.com/VarroaCI/varroa-jenkins/commit/057ca7d41f10d5fd5555371634762a97cd6e5c89))
* **operator:** write the approve status fields with a compare-and-swap ([9366591](https://github.com/VarroaCI/varroa-jenkins/commit/9366591129a88e4c8185b91261cc3bb319592aba))
* **pipelines-ui:** open the team run stream on the API base ([f0680cc](https://github.com/VarroaCI/varroa-jenkins/commit/f0680cc9e65b5c08a19857f3d0a4033f7c262e21))
* **pipelines:** accept the short-form job path and fix a tail-lease test flake ([d1ceef1](https://github.com/VarroaCI/varroa-jenkins/commit/d1ceef1a11fb0097c25ac5cb591fa1bf593c0728))
* **pipelines:** declare no job for a tenant its cell holds ([bf3f2a1](https://github.com/VarroaCI/varroa-jenkins/commit/bf3f2a18e3742a19018b49d8dac7ae0bb52aae61))
* **pipelines:** draw the stage graph at native size ([4a405ed](https://github.com/VarroaCI/varroa-jenkins/commit/4a405edc383bbe19f94c6ff86e077c63ded40501))
* **pipelines:** fit stage labels to their drawn width ([9fb0b08](https://github.com/VarroaCI/varroa-jenkins/commit/9fb0b085151d8212e7850fea396fb74a44d53333))
* **pipelines:** hide empty digests and interrupted log tails ([8bd940e](https://github.com/VarroaCI/varroa-jenkins/commit/8bd940e614fdf9ee97ca76acb12df0e46f2f8296))
* **pipelines:** hold declared jobs that the cell holds ([ea59598](https://github.com/VarroaCI/varroa-jenkins/commit/ea59598de5c4105725037ba158af4aa826e2de31))
* **pipelines:** keep team home and tests tables inside the page ([9e26fd5](https://github.com/VarroaCI/varroa-jenkins/commit/9e26fd57ca2a2b168c57d88f4108cf91cb90fdf2))
* **pipelines:** link dependency edges by folder-qualified job path ([221a537](https://github.com/VarroaCI/varroa-jenkins/commit/221a537cc36bc81dfe1bce773b3d7b6ef3b9a938))
* **pipelines:** list member-placed tenants on the pipelines landing page ([5c67ccd](https://github.com/VarroaCI/varroa-jenkins/commit/5c67ccd968a859c46e89ac9a75b67118091e0e96))
* **pipelines:** list only Teams the landing page can open ([16b484d](https://github.com/VarroaCI/varroa-jenkins/commit/16b484d86d4d187e435188af34da7f45824991bd))
* **pipelines:** name the current stage on the Running now tile ([1511fc7](https://github.com/VarroaCI/varroa-jenkins/commit/1511fc7f1650f9cf1abf913ca3a31f5a273212e0))
* **pipelines:** name the innermost failed stage in the digest and tail its own log ([902ef61](https://github.com/VarroaCI/varroa-jenkins/commit/902ef61086a4c199e8ded5d347632c1a7d05e05a))
* **pipelines:** name the script source of an inline-script job on the Defined by card ([4fea0c0](https://github.com/VarroaCI/varroa-jenkins/commit/4fea0c03d3b7ba7578f516f7d371733493054f45))
* **pipelines:** order branches by start, format the header duration, omit waitingSince on a held lock ([a59e3f7](https://github.com/VarroaCI/varroa-jenkins/commit/a59e3f7b32b0f7b2f102ae10ad28a3950c107b04))
* **pipelines:** place stage headers from line attribution ([e87a4f4](https://github.com/VarroaCI/varroa-jenkins/commit/e87a4f44a2eadaac4d55a7ce7b26dea62c7dff92))
* **pipelines:** read a member job's provenance and profile on the member ([08d40d9](https://github.com/VarroaCI/varroa-jenkins/commit/08d40d935bfa7f770191e9b7f1cb1da107f53a50))
* **pipelines:** read a member-placed Team's bundle live ([289b2a0](https://github.com/VarroaCI/varroa-jenkins/commit/289b2a0a1f7396f108e7207e7b0ca924d6a60b06))
* **pipelines:** read a placed Team's definition with the strict parser ([ece7772](https://github.com/VarroaCI/varroa-jenkins/commit/ece777278a66853c8743001914c1ecb31a09a8a2))
* **pipelines:** render every log frame and stop the stream on end ([d4796aa](https://github.com/VarroaCI/varroa-jenkins/commit/d4796aa0104f5186be7ba4b8798ae2f4f0517577))
* **pipelines:** show a refused run command on its run and keep its error short ([fdd9316](https://github.com/VarroaCI/varroa-jenkins/commit/fdd93167d66a7541f7675514a1d7cf56d4204928))
* **pipelines:** show member-cluster locks and member-cell placement columns ([927d348](https://github.com/VarroaCI/varroa-jenkins/commit/927d348dffdefc39c645b0e34d89e5349b29c9c6))
* **pipelines:** show the run's test totals in place of the empty suite table ([edab795](https://github.com/VarroaCI/varroa-jenkins/commit/edab795f551640f9ea76081fc41f7a1d455b095d))
* **pipelines:** show the waiting page for a team that has no cell ([ed249ac](https://github.com/VarroaCI/varroa-jenkins/commit/ed249acc98fc2e47d5bab51247be2ce081fbda01))
* **plugin,recordstore:** schedule nothing for a redelivered command of a started run ([66617a9](https://github.com/VarroaCI/varroa-jenkins/commit/66617a9a1dcbd18358ee02ced7cd5680ce783cdb))
* **plugin:** answer every plugin route refusal as plain text ([333664e](https://github.com/VarroaCI/varroa-jenkins/commit/333664e9e8dacee8d87500121fe52d44c1d162f6))
* **plugin:** block the jobDsl step at run time on a cell ([fbc63ea](https://github.com/VarroaCI/varroa-jenkins/commit/fbc63ea24c7fe5946b4d03348ac813d3842bd2e1))
* **plugin:** continue the log sequence of a resumed build ([956c396](https://github.com/VarroaCI/varroa-jenkins/commit/956c396c0db6970cade208d8c46b6ac9c9cc7461))
* **plugin:** emit the pod templates of a run that resolves no revision ([8fe53fb](https://github.com/VarroaCI/varroa-jenkins/commit/8fe53fb56eaf5cbe14ea4b4b16c0922d8130944e))
* **plugin:** end a build that cannot load its program with one completion ([f324fb7](https://github.com/VarroaCI/varroa-jenkins/commit/f324fb7815a41add0dcb1daaab36aaf159d78fb5))
* **plugin:** end a build that Jenkins refuses to resume with jenkins-restart ([f6446ae](https://github.com/VarroaCI/varroa-jenkins/commit/f6446aec03400d5dcdce7ed741a16508960fc649))
* **plugin:** hold a resumed lock body until the recorder confirms the hold ([55b5ffa](https://github.com/VarroaCI/varroa-jenkins/commit/55b5ffa7956d296b92f89daa16e179e53d8e8705))
* **plugininv:** name the plugin that declares the first optional edge ([9f1fcee](https://github.com/VarroaCI/varroa-jenkins/commit/9f1fcee5815fcc1db04d0d037be381b266dbfb13))
* **plugin:** keep the job path of each head in a branch scan ([667a5ba](https://github.com/VarroaCI/varroa-jenkins/commit/667a5ba1cd0cb7bf823ebe9c4f16c40b51b1bbba))
* **plugin:** load the varroa global variable class by name so a build resumes ([f5b50f1](https://github.com/VarroaCI/varroa-jenkins/commit/f5b50f1715252278296d9758f6dc3b803a729c6b))
* **plugin:** measure queue wait from the time the item entered the queue ([39fb106](https://github.com/VarroaCI/varroa-jenkins/commit/39fb10698539cf822aae96422ec8a1a99d97d9fc))
* **plugin:** name the actor in the cause of a Varroa-triggered build ([9cab155](https://github.com/VarroaCI/varroa-jenkins/commit/9cab1559b5c648a62476d70fa660fc4b00f042c7))
* **plugin:** name the reason on a refused claim and a failed cross-tenant step ([bdb003e](https://github.com/VarroaCI/varroa-jenkins/commit/bdb003e8d6914e506bac5ff2ae05c3cb5a516512))
* **plugin:** park a refused claim's retry off the claimer threads ([3e920b3](https://github.com/VarroaCI/varroa-jenkins/commit/3e920b34d0e7d913a85be6fe979cba508327e421))
* **plugin:** park the revision wait off the claimer threads ([2139121](https://github.com/VarroaCI/varroa-jenkins/commit/2139121b5b06b631787b6587092a4dc7d6bff4bb))
* **plugin:** pick a run's own checkout for its scm facts ([5ab2f83](https://github.com/VarroaCI/varroa-jenkins/commit/5ab2f833523f0b2ff222f1745064d41a9f216ed6))
* **plugin:** push the agent node config of a resumable build ([6f4d95d](https://github.com/VarroaCI/varroa-jenkins/commit/6f4d95d09ddb831022b953389f0fb00f3e36ab3a))
* **plugin:** resolve the run handle caller from the running pipeline ([d0f06ad](https://github.com/VarroaCI/varroa-jenkins/commit/d0f06adac5217caf1b6416ae55df0bb2ff8390fe))
* **plugin:** retry a NOT_READY claim at most every 10 seconds ([358ab8c](https://github.com/VarroaCI/varroa-jenkins/commit/358ab8c2b05bb616736b39d33aae53e792227eb5))
* **plugin:** send no second completion when a user unpauses a refused build ([df9d379](https://github.com/VarroaCI/varroa-jenkins/commit/df9d3795d87e3c3178fc0b766a3e688dfd56051e))
* **plugin:** strip credentials from a run's scm url ([858ac77](https://github.com/VarroaCI/varroa-jenkins/commit/858ac77fc7b9ecf794b3dcac84d8c8dc766e98d7))
* **profile:** name the session source and session start ([14fd4c2](https://github.com/VarroaCI/varroa-jenkins/commit/14fd4c298f3693f6730832b34c09f58a61b34786))
* **rbac:** report a failed delete of a stale federated JenkinsRole ([b60ae94](https://github.com/VarroaCI/varroa-jenkins/commit/b60ae94efb356cbd7893c03cbcebc2cec3a17183))
* **rbac:** report a failed delete of a stale federated JenkinsRoleBinding ([5f20b61](https://github.com/VarroaCI/varroa-jenkins/commit/5f20b616500dc105339039ae89e926a594f046e4))
* **rbac:** report the per-hive federation outcome on each VarroaRoleBinding ([e33ab24](https://github.com/VarroaCI/varroa-jenkins/commit/e33ab243a47427e07c1dc2afe06851a8ec443c1e))
* **rbac:** skip a federated binding with no subjects ([21602bb](https://github.com/VarroaCI/varroa-jenkins/commit/21602bb0c9fc2002e0fca50f3c542d3a507f6630))
* **recorder:** address the upstream fan-out and the backstop to the job's home slot ([7fbd77a](https://github.com/VarroaCI/varroa-jenkins/commit/7fbd77add5be5262041c4a44710aa26cf27eb5c8))
* **recorder:** bridge OTel counters onto /metrics and log refused claims ([e9c7a47](https://github.com/VarroaCI/varroa-jenkins/commit/e9c7a472a756e1f82525d34f2db44c8b4abc0777))
* **recorder:** end a marked run whose Jenkins restarts in a live pod ([b41d090](https://github.com/VarroaCI/varroa-jenkins/commit/b41d090df81eb1417bfdca6acc419be9ce069a91))
* **recorder:** end a marked run whose start event was lost ([497f741](https://github.com/VarroaCI/varroa-jenkins/commit/497f741264d470706ab44df4709f25c5e18bf1cc))
* **recorder:** end a silent run whose Jenkins restarts in a live pod ([811cee5](https://github.com/VarroaCI/varroa-jenkins/commit/811cee5838ceef253aa5e209ce5eebfb3f6d67a7))
* **recorder:** grant ledger-action publishes for every cluster and count permission violations ([d01f33f](https://github.com/VarroaCI/varroa-jenkins/commit/d01f33f8c55b7598fd8972a3c16a67e1a3ed168e))
* **recorder:** hold the run-state cap during a dispatched resume ([c535673](https://github.com/VarroaCI/varroa-jenkins/commit/c5356734a5b991860c73d2872691ad949a785470))
* **recorder:** keep a loss reason against a later complete log ([2ab4837](https://github.com/VarroaCI/varroa-jenkins/commit/2ab4837c9f86c016dcd2382ce1921f9a6ff6a80d))
* **recorder:** keep a lost run markable, free a torn-down tenant name, and announce the applied seed ([ac1e1e6](https://github.com/VarroaCI/varroa-jenkins/commit/ac1e1e68531315031185945e877aa1d129aeee1f))
* **recorder:** keep a teardown purge off a newer tenant's placement row ([2159a10](https://github.com/VarroaCI/varroa-jenkins/commit/2159a106fa8f9f51ee6ae25f7a3bda4a7dad3351))
* **recorder:** keep the most precise log incomplete reason ([3f31b2a](https://github.com/VarroaCI/varroa-jenkins/commit/3f31b2a7851f6d530a5b8cd33a1163965b1303d5))
* **recorder:** keep the run id on a forwarded build and resolve a new job's home at once ([10c850e](https://github.com/VarroaCI/varroa-jenkins/commit/10c850e57e8b5e7bfaaea17f2a95458e5f9d161c))
* **recorder:** let a cross-tenant caller read a vetoed run ([f5aa56a](https://github.com/VarroaCI/varroa-jenkins/commit/f5aa56acb63627842182b1c2166185acb7c8319b))
* **recorder:** let the backstop fire a home row below the placement generation ([2cff3d6](https://github.com/VarroaCI/varroa-jenkins/commit/2cff3d667813af9be2040876a765b7fc97719e81))
* **recorder:** mark a run TRIGGER_FAILED at once on a permanent claim veto ([cf7503d](https://github.com/VarroaCI/varroa-jenkins/commit/cf7503d765853fae313065cd04d81cf03c2fcea6))
* **recorder:** measure every liveness window on the recorder clock ([60e6010](https://github.com/VarroaCI/varroa-jenkins/commit/60e6010d6d1e83e220bbd80a2cae6e79410d0285))
* **recorder:** move a job's schedule rows when its home slot moves ([4e33e0b](https://github.com/VarroaCI/varroa-jenkins/commit/4e33e0b2a038d5ddb0e011c499333fdcd3fbc2b9))
* **recorder:** name the holder cell when a force release finds a live pod ([6cdbb26](https://github.com/VarroaCI/varroa-jenkins/commit/6cdbb261abc92cf2021d5c898906dde1fa534c72))
* **recorder:** publish no run notification for a run.state envelope ([96f77d9](https://github.com/VarroaCI/varroa-jenkins/commit/96f77d9e48d7526778eb999d9c72fa217ef60490))
* **recorder:** raise the liveness stamps only for a run's own owner ([18577de](https://github.com/VarroaCI/varroa-jenkins/commit/18577debd8c827580bdcbd06b400a03b12138fd0))
* **recorder:** re-fire a backstop trigger that reached no Jenkins ([d8b7e14](https://github.com/VarroaCI/varroa-jenkins/commit/d8b7e14d65a0ae541d9f1842f8060f794d722e69))
* **recorder:** refresh a stale build number on a repeat claim from a new cell ([7ad0493](https://github.com/VarroaCI/varroa-jenkins/commit/7ad049368f8f33c606a0534199a1012708571524))
* **recorder:** refuse a late lock acquire for an ended run ([75859e5](https://github.com/VarroaCI/varroa-jenkins/commit/75859e5056ec17dc969965bc6451900848671aaf))
* **recorder:** release fleet lock rows of ended and interrupted runs at once ([c0c6cea](https://github.com/VarroaCI/varroa-jenkins/commit/c0c6cea4d4fe16e035d9f6b0f48f36f71c5f80d0))
* **recorder:** send the automatic retry of an interrupted run to the job's home slot ([48ea9fd](https://github.com/VarroaCI/varroa-jenkins/commit/48ea9fd8c4ee13292e23f0930376eaaa994cfbf6))
* **recorder:** subscribe again after NATS denies a responder ([06161ab](https://github.com/VarroaCI/varroa-jenkins/commit/06161ab04cfb915a714646e86f8d1ad308a37d7a))
* **recordstore:** drop the unused run_stages log range columns ([b7928e7](https://github.com/VarroaCI/varroa-jenkins/commit/b7928e7baa84891d14f41afd31d2bb5941d22c03))
* **recordstore:** fire the completion webhook for synthetic run ends ([7691d9b](https://github.com/VarroaCI/varroa-jenkins/commit/7691d9b277785055d4ed398a251888d7f01a6867))
* **recordstore:** hold a repeat claim while another pod can still build the run ([4ce9487](https://github.com/VarroaCI/varroa-jenkins/commit/4ce94877f797056bbefde3aa7646bde605636288))
* **recordstore:** name the refused signature in a failure webhook ([847d549](https://github.com/VarroaCI/varroa-jenkins/commit/847d549b2a449fa8de65ac013f462fe4ad228478))
* **recordstore:** publish a wait notification when a cap wait changes ([eb4c091](https://github.com/VarroaCI/varroa-jenkins/commit/eb4c0910f4f4a324cb048c3d57d9235a4a626608))
* **recordstore:** refuse a retired incarnation and drop the dead entry of a named pod ([7c2f09f](https://github.com/VarroaCI/varroa-jenkins/commit/7c2f09f942a1541f8ff6f1c648f287492fe59792))
* **recordstore:** refuse a stale tenant announcement ([6c386c5](https://github.com/VarroaCI/varroa-jenkins/commit/6c386c509292516cc83eb22f6411ae38f83c86cb))
* **recordstore:** release a fleet lock only on a confirmed or rolled dead instance ([082e8e0](https://github.com/VarroaCI/varroa-jenkins/commit/082e8e084261b5acf1c47fe87bbceb5879ef816e))
* **recordstore:** resolve a fleet lock owner from a separate tenant field ([8ebdd7d](https://github.com/VarroaCI/varroa-jenkins/commit/8ebdd7d438a200bf9878fc6d6404ade3cfd73e2a))
* **recordstore:** retire the incarnation of a deleted Team on its teardown purge ([ed86e61](https://github.com/VarroaCI/varroa-jenkins/commit/ed86e6137e05f060604c940c0cdccc2cc4de5ca7))
* **recordstore:** stamp an upstream link only from a recorder-written origin ([33526a8](https://github.com/VarroaCI/varroa-jenkins/commit/33526a87c5f2eb733b097f7de761843180cbf609))
* **recordstore:** stop grants tests from dropping a shared Postgres role ([1f9dc82](https://github.com/VarroaCI/varroa-jenkins/commit/1f9dc828fb3b5e9428bf87982062e339f7a05305))
* **recordstore:** store the caller job path on a fleet lock row ([edd4268](https://github.com/VarroaCI/varroa-jenkins/commit/edd42682fbca80ae2cc24d181e35e8a76e40a533))
* render pipeline quietPeriod/resumeBlocked, hide internal artifacts, record every pod template ([31ec12a](https://github.com/VarroaCI/varroa-jenkins/commit/31ec12a89297bbab1d9bd634f5a5c5f570ca3e0f))
* **restart:** replace a rehydrated build stub with the restart copy ([9477987](https://github.com/VarroaCI/varroa-jenkins/commit/94779872eccd4e080307374730ce0dc69f71b469))
* **scaletest:** record aborts, refuse stale results, add resume ([4b19d08](https://github.com/VarroaCI/varroa-jenkins/commit/4b19d0851b7f2e9262fb2dc12e73a6b86552ea98))
* **schedules:** refresh the cron horizon as occurrences pass and keep a cell's rows across a roll ([bfc758c](https://github.com/VarroaCI/varroa-jenkins/commit/bfc758c92f8328e8f3b85b9dc1384e406d5fc26d))
* **teams:** keep the slot counts on a form save ([4e09023](https://github.com/VarroaCI/varroa-jenkins/commit/4e090235220dd4792fab0b16f19054270f45325f))
* **transport:** release the inbound subscription of a deleted controller ([2eeafac](https://github.com/VarroaCI/varroa-jenkins/commit/2eeaface0cbddc304184131e87db05cdb3040e5c))
* **transport:** subscribe again when NATS rejects a mite inbound subscription ([3deafec](https://github.com/VarroaCI/varroa-jenkins/commit/3deafecd4ff8c0814ce42c53f8a9e0e8a9b35d4e))
* **updatecenter:** check latest pins of a two-segment version against its core ([c1aabf5](https://github.com/VarroaCI/varroa-jenkins/commit/c1aabf53adc09ee55f10dba663d4f5fc06f7cab3))
* **updatecenter:** gate Ready on servability, not store coverage ([c6150b7](https://github.com/VarroaCI/varroa-jenkins/commit/c6150b7980fd11340ab8a3da84465d09685a672f))
* **updatecenter:** reject a latest pack version at import and follow the request context in fleet plugin reads ([b98108a](https://github.com/VarroaCI/varroa-jenkins/commit/b98108a725965d2e6500db22d0ab8d48df76f7ec))
* **updatecenter:** resolve a latest pin to the advertised version ([d14ca71](https://github.com/VarroaCI/varroa-jenkins/commit/d14ca71c71774c416855467610351d03d17f28c9))
* **updatecenter:** score a latest declaration by the version it resolves to ([028ccd1](https://github.com/VarroaCI/varroa-jenkins/commit/028ccd11a16ad63a097f650a9bbd0be820e0567b))
* **updatecenter:** serve each plugin's dependencies in the update-center metadata ([c1375b5](https://github.com/VarroaCI/varroa-jenkins/commit/c1375b5132175d90275dd5302ded01b22f163f23))
* **updatecenter:** treat a missing dependency list as unknown and report it ([e8dc689](https://github.com/VarroaCI/varroa-jenkins/commit/e8dc689bfe7fe5d2bc724cfc993690e29f50b394))
* **updatecenter:** use one latest predicate and resolve latest before export ([e7c9e8f](https://github.com/VarroaCI/varroa-jenkins/commit/e7c9e8f9e42a8c7991046d2ee3e9a06b01145a45))
* **varroactl:** let the login callback finish its page before the server stops ([a61cd93](https://github.com/VarroaCI/varroa-jenkins/commit/a61cd93a1c4dc139fd21dbce7a9a8ccededb61f2))
* **varroactl:** resume a run-log follow after the last printed line ([931e7f8](https://github.com/VarroaCI/varroa-jenkins/commit/931e7f81e2412cedc4c1d3d811882ef61614090d))


### Performance Improvements

* **bff:** serve local controller lists from the informer cache ([8639a75](https://github.com/VarroaCI/varroa-jenkins/commit/8639a75c581bd2e96b58164b6f814c736276864f))
* **frontend:** bound the dashboard and controllers list at fleet scale ([51d4a99](https://github.com/VarroaCI/varroa-jenkins/commit/51d4a99c1e4864528bc7bfa77de41af764555a8a))
* **operator:** push desired state in the pass that moves a reconnect to Connected ([6cb1c2c](https://github.com/VarroaCI/varroa-jenkins/commit/6cb1c2c28c393159f9cf08642b298d09ec4c254e))

## [0.3.0](https://github.com/VarroaCI/varroa-jenkins/compare/v0.2.0...v0.3.0) (2026-09-01)


### Features

* in-cluster Jenkins upgrade tracking, promotion, and rollout ([e1325e6](https://github.com/VarroaCI/varroa-jenkins/commit/e1325e63a8054582d30b45578551d2e58ec2dab6))

## [0.2.0](https://github.com/VarroaCI/varroa-jenkins/compare/v0.1.0...v0.2.0) (2026-08-30)


### Features

* dual-license under AGPL-3.0 and commercial terms ([aea64fb](https://github.com/VarroaCI/varroa-jenkins/commit/aea64fb7662c5d10c7ea80a5b5e94de65da53872))


### Bug Fixes

* operator and update-center bug squash ([4484572](https://github.com/VarroaCI/varroa-jenkins/commit/4484572264ba908df5b83b308dae36e416b79d53))

## 0.1.0

Initial public release.

### Update center

- Resolve aged plugin pins by falling back to the artifact-archive `.sha256`
  sidecar when update-center metadata no longer carries the checksum, and
  authenticate seed refs when pulling seed packs from a private registry.
- Include the `/plugins` path segment in the in-cluster update-center download
  URL, so Jenkins can fetch `.hpi` blobs from it.
- Make the archive checksum fallback host configurable
  (`updateCenter.pullThrough.archiveURL`), so an egress-restricted install can
  turn it off.
- Plugin packs now carry the full dependency closure instead of only the
  top-level plugin list.

### Controllers and the operator

- `ControllerSpec` is projected through the API and server-side applies are
  completed from the manager's ownership set. **Breaking**: the API surface for
  controller spec fields changed.
- The operator no longer claims ownership of `Controller` spec fields it does
  not own, so external tools can edit them without being reverted.
- `executeGroovy` targets now fail when the script fails to compile or throws,
  instead of reporting success.
- The gateway retries failed bus watches and surfaces a starved mite rather
  than hanging.
- A superseded mite stream no longer dispatches inbound messages.
- Update-center sync no longer starves bundle recomposition — the two share a
  reconcile queue and long syncs used to block it.

### Dashboard

- State-aware controller detail redesign, organised around
  Bundle → Apply → Jenkins.
- Free-form maps are editable in the curated spec form.
- Named SSE frames are delivered to consumers, and terminal-status streams shut
  down cleanly.
- Readability and alignment passes across the dashboard, filter bar, wizard
  card, and key/value grids.
- The install wizard supplies and validates the dashboard host for path-mode
  ingress.

### MCP tools

- Every mutating MCP tool audited; results sanitised, listings projected to
  summaries, and all tools classified.
- Composed-bundle dry-run tools, OCI blob fetch, and catalog source filtering
  added or fixed.
- Every mutable controller spec field is now reachable from create/update, and
  there is a documented way past a server-side-apply field conflict.

### Chart and packaging

- Released charts pin the component images to the chart version instead of
  tracking `latest`.
- Pod placement controls for operator, gateway, BFF, Dex, and NATS.
- The rolled-in Prometheus and Grafana subcharts were removed — bring your own
  observability stack.

### CLI

- `varroactl` treats a registry port as a port rather than a tag.
