// The application shell — it proves the toolchain boots (router, Pinia, a
// rendered route), nothing more. The console itself is not built yet.
import { createPinia } from "pinia";
import { createApp } from "vue";

import App from "./App.vue";
import router from "./router";

createApp(App).use(createPinia()).use(router).mount("#app");
