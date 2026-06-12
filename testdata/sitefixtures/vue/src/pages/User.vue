<script setup>
import { useRoute } from "vue-router";
const route = useRoute();
</script>

<template>
  <section>
    <h1>vue spa user {{ route.params.id }}</h1>
    <router-link to="/">home</router-link>
  </section>
</template>
