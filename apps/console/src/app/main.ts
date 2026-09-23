// Browser entry point: Pinia and the router are installed once, at the
// application boundary. Product state arrives in `stores/` with the product
// domain that owns it; the removed counter was only starter proof, not state.
import { createPinia } from "pinia";
import { createApp } from "vue";

import App from "./App.vue";
import router from "@/router";
import "@/styles/main.css";

createApp(App).use(createPinia()).use(router).mount("#app");
