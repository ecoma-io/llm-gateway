// Browser entry point: Pinia and the router are installed once, at the
// application boundary. Product state arrives in `stores/` with the product
// domain that owns it; the removed counter was only starter proof, not state.
//
// The 401 watcher is subscribed HERE, at the one place that exists before any
// screen mounts. The seam announces the first 401 of any call, and a subscriber
// installed later would miss a 401 from the very first page — which is the one
// that follows an expired session, the case the announcement exists for.
import { createPinia } from "pinia";
import { createApp } from "vue";

import App from "./App.vue";
import router, { watchForSessionEnd } from "@/router";
import "@/styles/main.css";

watchForSessionEnd(router);

createApp(App).use(createPinia()).use(router).mount("#app");
