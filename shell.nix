{ pkgs ? import <nixpkgs> {}
}:

with pkgs;
stdenv.mkDerivation {
  name = "dev-shell";
  buildInputs = [ go_1_21 ];
}
