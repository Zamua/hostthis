<script>
  import { Router, Route } from "svelte-routing";
  import Home from "./pages/Home.svelte";
  import About from "./pages/About.svelte";
  import User from "./pages/User.svelte";
</script>

<Router>
  <Route path="/" component={Home} />
  <Route path="/about" component={About} />
  <Route path="/users/:id" let:params>
    <User id={params.id} />
  </Route>
</Router>
