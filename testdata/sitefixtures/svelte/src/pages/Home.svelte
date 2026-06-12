<script>
  import { Link } from "svelte-routing";
</script>

<section>
  <h1>svelte spa home</h1>
  <p>client-side routing fixture for the hostthis SPA fallback</p>
  <nav>
    <Link to="/about">about</Link>
    <Link to="/users/123">user 123</Link>
  </nav>
</section>
