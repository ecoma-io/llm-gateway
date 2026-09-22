// The router is functional from day one, so every later page is a route
// addition rather than a wiring change.
import { createRouter, createWebHistory } from "vue-router";

import HomeView from "@/views/HomeView.vue";

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: "/",
      name: "home",
      component: HomeView,
    },
    // Static import, not lazy: lazy loading earns its complexity with a
    // second route, and this file has one.
  ],
});

export default router;
