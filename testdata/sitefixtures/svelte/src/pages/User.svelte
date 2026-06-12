<script>
  import { Link } from "svelte-routing";
  export let id;
</script>

<section>
  <h1>svelte spa user {id}</h1>
  <Link to="/">home</Link>
</section>
