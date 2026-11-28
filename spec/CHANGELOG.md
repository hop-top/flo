# Changelog

## 0.1.0-alpha.0 (2026-11-28)


### ⚠ BREAKING CHANGES

* **spec:** Track type, Flow.Track field and top-level track key removed; a document with track: fails as unknown key. Carry such data under an x- key.

### Features

* **inbox:** person guard from script sender facts ([88bc696](https://github.com/hop-top/flo/commit/88bc6960a83c7d6befab72abd39424d7d61e7e14))
* **node:** auth.optional skips the auth header when the secret is absent ([55997aa](https://github.com/hop-top/flo/commit/55997aaa22a49b6f35e31b8cc63730b27a25840a))
* **spec:** admission namespace in node templates ([3789705](https://github.com/hop-top/flo/commit/3789705af3b603abcaadd9cea9b0f5d80105685b))
* **spec:** CEL compile + eval in spec/cel ([40fd766](https://github.com/hop-top/flo/commit/40fd76659bc9cf2b4ee42a0818c31c958eb2595f))
* **spec:** cel RenderValue, typed whole-placeholder walk ([e4d34dd](https://github.com/hop-top/flo/commit/e4d34dddb68e4160705791c99945aeb499c932cc))
* **spec:** declare out in the cel env ([ada6488](https://github.com/hop-top/flo/commit/ada6488594ed2472c35ecaf4d4719a4b39912bc3))
* **spec:** export step walk, copy, ref rewrite, template helpers ([497f95d](https://github.com/hop-top/flo/commit/497f95d6c07012ee3514632490e453b8a7995cf1))
* **spec:** extract plan-level flow code into hop.top/flo/spec ([1cb1ad5](https://github.com/hop-top/flo/commit/1cb1ad5548cb0cc79e75fa4372baf01af045f4a5))
* **spec:** flow x node validation V11 V12 V19 V20 V22 V24 V25 V27 ([6850cc9](https://github.com/hop-top/flo/commit/6850cc9d1bdc197dc067a96db6e0f9dcd4b390f6))
* **spec:** key aliases, raw hash, layered fs locator ([bab3d38](https://github.com/hop-top/flo/commit/bab3d38901ea66aabec1a1b405714b80996f4f7e))
* **spec:** lower declared command nodes to exec steps ([cbbb299](https://github.com/hop-top/flo/commit/cbbb299113709d95671654132952012c65dfdcf8))
* **spec:** make the in namespace usable in cel expressions ([c163339](https://github.com/hop-top/flo/commit/c163339f1c4fdb174399d57a18b96607bdf079b5))
* **spec:** node spec types + strict decoder ([eddd09f](https://github.com/hop-top/flo/commit/eddd09fb0e03dadb3831b2b68d114200ea0d9947))
* **spec:** node validator N03-N15, packs, trust providers, locator layers ([7bd246f](https://github.com/hop-top/flo/commit/7bd246f68d40eece300ce1d700efec226c4a08e3))
* **spec:** reserve the admission name ([c6e6b32](https://github.com/hop-top/flo/commit/c6e6b326bc73370589ab96c0eeb69b4a5cdbbb98))
* **spec:** retention.recordings flow key ([a4e134a](https://github.com/hop-top/flo/commit/a4e134a1e994c1e50217229b114806e2293db429))
* **spec:** run-level rules V05 V07 V16 V17 V25 V26, typed --var coercion ([ce11292](https://github.com/hop-top/flo/commit/ce112929d39070d8505562b4635abd4682638bd7))
* **spec:** run-level types + Decode for on/concurrency/budget/state/tasks/privacy ([162b6a4](https://github.com/hop-top/flo/commit/162b6a40fb36d6c97d304b75d6ce6cc4ece06248))
* **spec:** StepFields, IsLegacyCondition ([e2017ef](https://github.com/hop-top/flo/commit/e2017ef10a7ebc527a08e86f0a19728582ced75e))
* **spec:** V19 reads model lists; jev shorthand is cloud ([b9ba5bb](https://github.com/hop-top/flo/commit/b9ba5bbf503a5e33c6888a4e8ad1e97b6f0cd890))
* **spec:** x- extension fields on flow and step; drop track ([9eee8ac](https://github.com/hop-top/flo/commit/9eee8ac75a23f966cd3f8b0586aea57a3acbe85d))


### Bug Fixes

* **spec:** *.localhost is a named host, not local ([a4292b3](https://github.com/hop-top/flo/commit/a4292b3907d4b8ff601a450d4d1fdfcc4244634d))
* **spec:** dotted slashless ollama authority is cloud ([c636f56](https://github.com/hop-top/flo/commit/c636f56514a51aba4028ee14e864ae282029b728))
* **spec:** Expand accepts declared state reads ([7be83ab](https://github.com/hop-top/flo/commit/7be83abc7466bc05baf170f246fe285eb1f440ab))
* **spec:** model privacy follows model client endpoint precedence ([35f8cd1](https://github.com/hop-top/flo/commit/35f8cd1fa4436b96f042ef255342b3fcbbe45b89))
* **spec:** ollama model is local only on a local host ([4faa507](https://github.com/hop-top/flo/commit/4faa5077d2267ddb07752f420d551b9569e7a5ab))
