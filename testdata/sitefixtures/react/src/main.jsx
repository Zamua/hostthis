import React from "react";
import ReactDOM from "react-dom/client";
import {
  BrowserRouter,
  Routes,
  Route,
  Link,
  useParams,
} from "react-router-dom";

function Home() {
  return (
    <section>
      <h1>react spa home</h1>
      <p>client-side routing fixture for the hostthis SPA fallback</p>
      <nav>
        <Link to="/about">about</Link>
        <Link to="/users/123">user 123</Link>
      </nav>
    </section>
  );
}

function About() {
  return (
    <section>
      <h1>react spa about</h1>
      <p>this route has no real file on disk; the SPA fallback serves index.html</p>
      <Link to="/">home</Link>
    </section>
  );
}

function User() {
  const { id } = useParams();
  return (
    <section>
      <h1>react spa user {id}</h1>
      <Link to="/">home</Link>
    </section>
  );
}

ReactDOM.createRoot(document.getElementById("root")).render(
  <React.StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/about" element={<About />} />
        <Route path="/users/:id" element={<User />} />
      </Routes>
    </BrowserRouter>
  </React.StrictMode>
);
